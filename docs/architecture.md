# mere — architecture

How the server is built, as shipped. For the request/response contracts see
[api.md](api.md); for the extension seams and how to build on them see
[extending.md](extending.md). This document describes the system as it
actually exists in the code.

## Shape

One Go binary, two databases.

```
        HTTP clients (snippet · server SDK · MCP client · browser)
                                  │
                                  ▼
        ┌─────────────────────────────────────────────────┐
        │            mere-server (single binary)           │
        │                                                  │
        │   Web UI        Public API        OAuth 2.1      │
        │   (templ+htmx)  /api/v1/*         /oauth/*       │
        │                 /mcp                             │
        │                                                  │
        │   ingest pipeline · query executor · auth/viewer │
        └───────────────┬──────────────────┬───────────────┘
                        │                  │
                        ▼                  ▼
                 ┌────────────┐     ┌──────────────┐
                 │  Postgres  │     │  ClickHouse  │
                 │ (operational│    │  (analytics) │
                 │   state)   │     │ events/persons/
                 │            │     │ sessions views│
                 └────────────┘     └──────────────┘
```

No worker process: all async work (the ingest flusher, the DLQ drainer) runs as
goroutines inside the server. A separate one-shot `mere-maintenance` binary
sweeps expired auth rows on a cron.

## Request layers

Every request passes through `recover → log → authMiddleware`. `authMiddleware`
resolves the session cookie, attaches a per-request `*Viewer` (the membership-
scoped data-access object), enforces CSRF on non-GET web routes, and runs the
`must_change_password` gate. From there:

- **Web UI routes** wrap `requireSession` (or `requireAnonymous` for `/login`).
- **`/api/v1/*`, `/mcp`, `/api/v1/whoami`** wrap `RequireBearer` (OAuth) + `CORS`,
  bypassing the cookie/CSRF machinery entirely.
- **`/api/v1/ingest/events`** wraps `requirePublicToken` + `MaxBody` + `CORS`.

Once the tenant is resolved on the ingest and query/MCP chains, an
`extension.RateLimiter` seam is consulted (see [extending.md](extending.md)); the open-source build ships
the no-op allow-all default, so there is no rate limiting here — only a generic
extension point. A denied request gets `429` + `Retry-After` and the handler
never runs.

Routing is stdlib `net/http` with the Go 1.22+ pattern mux — no third-party
router. The whole HTTP surface is assembled in `internal/web/server.go`.

## Authentication

Three credential types, each for a distinct caller:

| Surface | Credential | Store |
|---|---|---|
| Web UI | Session cookie (`HttpOnly`, `SameSite=Lax`), 7-day sliding / 30-day hard cap | `sessions` (Postgres) |
| `POST /api/v1/ingest/events` | Public per-project token `mere_pub_…` (non-secret; lives in client HTML) | `api_tokens` (sha256 hash) |
| `/api/v1/*`, `/mcp` | OAuth 2.1 access token (PKCE, 1h TTL, no refresh) | `oauth_access_tokens` (sha256 hash) |

The OAuth server is in-process (`internal/oauth/`, handlers in
`internal/web/`): RFC 7591 dynamic client registration, S256-only PKCE,
one-shot 10-minute authorization codes, and access tokens bound to one
`(user, project)` pair chosen at the consent screen. There is no admin role —
authority comes entirely from team membership, enforced by the `Viewer`.

## Data model

**Postgres** holds operational state: `users`, `teams`, `team_memberships`,
`projects` (soft-deletable via `deleted_at`), `api_tokens`, `team_invites`,
`sessions`, the OAuth tables (`oauth_clients`, `oauth_codes`,
`oauth_access_tokens`), and `failed_events` (the ingest DLQ). IDs are UUID v7
(time-sortable), generated app-side. SQL is `sqlc`-generated typed Go from
`internal/postgres/queries/`.

**ClickHouse** holds analytics. Ingest writes to the hidden landing table
`events_raw_v1`; the public query surface is the curated trio `events`,
`persons`, and `sessions`. `events` is a plain view that resolves identity at
read time. `persons` and `sessions` are public views over hidden materialized
aggregate state tables. `properties` and `extras` are stored as JSON
**strings**; query them with `JSONExtract*`. `project_id` is the leading key on
the physical tables, so project scoping is cheap at the part level.

Two ClickHouse users, both app-managed:

- **`mere_admin`** — full DDL/DML; runs migrations and ingest writes. Defined in
  `config/deploy/clickhouse/users.xml`.
- **`mere_readonly`** — `SELECT` only (`readonly=2`); used for all query reads.
  Provisioned idempotently by the app on every boot
  (`internal/clickhouse.ProvisionReadonlyUser`), the single source of truth.

## Ingest pipeline

`internal/ingest`. `POST /api/v1/ingest/events` validates the batch (required `event` +
`timestamp`; optional `anonymous_id`, `user_id`, `session_id`;
`properties`/`extras` default to `{}`; per-event rejection reasons), pushes
valid events onto a buffered channel, and returns `202` immediately. A
background flusher drains the channel and writes one batched `INSERT` to
`events_raw_v1` as `mere_admin`, flushing on a size or time trigger.

`$identify` events with `anonymous_id` and `user_id` populate
`identity_links_v1` through a ClickHouse materialized view. Public
`distinct_id` columns resolve to `user_id` when known, otherwise
`anonymous_id`, so pre-identification events can be queried under the later
identified user without rewriting raw event rows.

Reliability is layered:

- **CH write fails** → the batch is written to the `failed_events` table in
  Postgres (the DLQ). A second goroutine drains the DLQ back into ClickHouse and
  deletes rows on success; it's a retry buffer, not an audit trail.
- **CH *and* the DLQ both fail in one flush** → a process-level **fatal flag**
  is set: `/api/v1/ingest/events` and `/healthz` return `503` until the next successful
  write clears it. The producer keeps retrying; nothing is dropped silently.
- **Buffer saturated** → `503` + `Retry-After: 1`.
- **SIGTERM** → ingest stops accepting, the channel drains to CH within
  `INGEST_SHUTDOWN_GRACE`, and any residual events are written to the DLQ before
  exit. `kamal-proxy`'s `drain_timeout` (30s) is set to exceed this budget.

After a batch durably lands in ClickHouse — at the primary flush or, for events
that first failed, at the successful DLQ drain — an `extension.UsageSink` seam is
notified with `(projectID, eventCount)` so a hosted build can meter per-tenant
volume for billing. Each event is counted exactly once, at its first
successful insert; the open-source build ships the no-op discard default, so it
counts nothing.

ClickHouse-side `async_insert` (configured in `config.xml`) batches small
inserts again at the server; the two layers are complementary.

## Query + isolation

`internal/query`. The executor sends the user's SQL to ClickHouse
**unmodified** and attaches the `additional_table_filters` session setting,
which transparently injects `project_id = '<grant>'` into every reference to
the hidden physical tables scanned by the public views: `events_raw_v1`,
`identity_links_v1`, `persons_state`, and `sessions_state`. This matters because
ClickHouse materialized views write to separate state tables; filtering only the
raw landing table would not isolate `persons` or `sessions`. There is no SQL
parser or CTE rewriter to audit. The connection runs as `mere_readonly`, and
per-request limits (`max_execution_time`, `max_memory_usage`, `max_result_rows`)
are attached the same way. By default queries have 60 seconds and 1000 result
rows, tunable with `QUERY_MAX_EXECUTION_TIME` and `QUERY_MAX_RESULT_ROWS`.
Results stream to the client rather than buffering.

**Single executor, two front doors.** The HTTP query/schema handlers and the MCP
`query`/`schema` tools both call the same `internal/query` executor and schema
provider — the tenant filter and the table allowlist live in exactly one place,
so they cannot drift. A `tenant_isolation_test.go` enrolls every read surface
and asserts project A's token never surfaces project B's data.

## MCP

`internal/mcp` mounts a stateless Streamable HTTP MCP server at `/mcp` behind the
same bearer + CORS middleware as `/api/v1/*`. The `query` and `schema` tools take
no `project_id` — the project comes from the token grant — and re-check project
visibility before touching ClickHouse. ClickHouse/validation problems surface as
**tool errors** (the model can self-correct); only panics or infrastructure
failures become JSON-RPC `internal_error`. Tool handlers are wrapped in
double panic-recovery so a library bug can't crash the process.

## Boot sequence

The boot sequence and the three-phase SIGTERM choreography live in the
importable `app` package (`app.Build` / `app.Run`); `cmd/server/main.go` is a
thin shim that constructs the logger and forwards its `Version` stamp.
`app.Build`:

```
env → config.Load → pg.Open → migrate.Run(pg) →
  ch.OpenAdmin → ch.CreateDatabase → ch.ProvisionReadonlyUser →
  migrate.Run(ch) → ch.OpenReadonly → start ingest → http.Serve
```

Failure at any step aborts startup; `kamal-proxy` keeps routing to the prior
version. The build-time `Version` (`git describe`, injected via `-ldflags` into
`cmd/server` and passed through `app.WithVersion`) is logged on boot and
returned in the `/healthz` body. Promoting the wiring into `app` lets a separate
wrapper module compose the same boot sequence and inject the `extension` seams
without forking (see [extending.md](extending.md)).

## Deploy

Kamal, build-and-push from the operator's machine (no CI image pipeline). The
multi-stage `Dockerfile` produces a static `mere-server` (plus
`mere-maintenance`) on `alpine`, running as a non-root user; migrations are
embedded, so nothing extra is copied into the runtime image. Postgres and
ClickHouse run as Kamal accessories with data on host volumes under
`/data/mere/`. See [self-host.md](self-host.md).

## Package map

| Package | Responsibility |
|---|---|
| `cmd/server` | Entry-point shim; forwards the build-time `Version` into `app`. |
| `cmd/maintenance` | One-shot expired-row sweeper. |
| `app` | Importable composition root: boot sequence + SIGTERM choreography; seam injection points. |
| `extension` | Exported in-process extension seams (`RateLimiter`, `UsageSink`) + no-op defaults; the only non-`internal/` package. |
| `internal/web` | HTTP handlers, middleware, route assembly. |
| `internal/auth` | Sessions, password hashing, CSRF, tokens, the `Viewer`. |
| `internal/oauth` | OAuth 2.1 server: clients, codes, access tokens, PKCE. |
| `internal/ingest` | Validation, buffered channel, flusher, DLQ drainer, state flags. |
| `internal/query` | Query executor + schema provider (the one isolation chokepoint). |
| `internal/mcp` | MCP transport, tools, panic-recovery adapter. |
| `internal/clickhouse` | CH pools (admin + readonly), readonly-user provisioning. |
| `internal/postgres` | PG pool + `sqlc`-generated queries. |
| `internal/migrate` | `golang-migrate` runner with dirty-state guidance. |
| `internal/config` | Env-var config struct + validation. |
| `internal/views` · `internal/static` | templ templates + embedded assets. |
| `internal/maintenance` | The sweep logic behind `cmd/maintenance`. |
