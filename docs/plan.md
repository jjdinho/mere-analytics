# Analytics Server — Plan

**Status:** Draft for review
**Last updated:** 2026-05-29

## Purpose

A small, self-hostable analytics server written in Go. Three jobs:

1. **Ingest** events from anywhere (snippet, server-to-server, CLI, agents).
2. **Store** them in ClickHouse with project-level scoping enforced at the app layer.
3. **Expose** them via a stable, versioned HTTP API + an MCP endpoint, plus a small web UI.

Self-hosters get the full product. No "cloud-only" features in this repo.

## Non-goals (in this repo)

- **No SDK.** The browser snippet lives in a different repo.
- **No CLI.** Lives in a different repo.
- **No SaaS overlay.** No billing, no per-tenant query budgets, no multi-region routing, no rate limiting. The deployer owns ops.
- **No agent / no LLM.** Agents are consumers of the API, not part of it.
- **No admin role.** Every account is a regular user scoped to projects they own or are invited to. The deployer (with shell access to the server) is the "operator" — that's external to the app, not an app role.
- **No dashboards, signals, recordings, triggers, smart events, page catalog, third-party integrations.** Consumers build these on top.
- **No identity resolution.** Caller supplies `distinct_id`. Events without one are stored with `distinct_id = NULL`.
- **No cookies** outside the web UI. Web UI uses a session cookie for login; the snippet (separate repo) does not.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                       HTTP clients                           │
│   (browser snippet, server SDKs, MCP clients, CLI, agents)  │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                    Go server (single binary)                 │
│                                                              │
│  ┌────────────────────┐    ┌───────────────────────────┐   │
│  │   Public API       │    │   Web UI                  │   │
│  │   (JSON)           │    │   (htmx + templ)          │   │
│  │   - /ingest/v1/*   │    │   - signup / login        │   │
│  │   - /api/v1/       │    │                           │   │
│  │     projects/*     │    │   - query playground      │   │
│  │   - /mcp           │    │                           │   │
│  └─────────┬──────────┘    └────────────┬──────────────┘   │
│            │                             │                  │
│  ┌─────────▼─────────────────────────────▼──────────────┐  │
│  │  Ingest pipeline     Auth          Query executor    │  │
│  │  (batch → CH)        (PG)          (CH filter setting)│  │
│  └──────────────────────────────────────────────────────┘  │
└──────────────────────────┬──────────────────────────────────┘
                           │
              ┌────────────┴────────────┐
              ▼                         ▼
       ┌──────────────┐         ┌──────────────────┐
       │  Postgres    │         │   ClickHouse     │
       │              │         │                  │
       │  users,      │         │  events_raw_v1   │
       │  teams,      │         │  queryable via   │
       │  projects,   │         │  /api/v1/...     │
       │  api_tokens, │         │                  │
       │  oauth_*,    │         │                  │
       │  sessions,   │         │                  │
       │  failed_evts │         │                  │
       └──────────────┘         └──────────────────┘
```

One binary, two databases. No worker process in v1 — async work runs as goroutines in the same process.

## Tech stack

### Decided

| Layer | Choice |
|---|---|
| Language | Go (latest stable) |
| HTTP server | `net/http` (Go 1.22+ router is sufficient for this surface) |
| ClickHouse driver | `github.com/ClickHouse/clickhouse-go/v2` (official) |
| Postgres driver | `github.com/jackc/pgx/v5` |
| Templating | `templ` (compiled Go templates) |
| Frontend interactivity | htmx |
| MCP server | `github.com/mark3labs/mcp-go` |
| OSS license | Apache 2.0 |

### Open (recommendations marked)

| Layer | Options | Recommendation |
|---|---|---|
| SQL access (Postgres) | `pgx` directly · `sqlc` · `sqlx` · `gorm` | **`sqlc`** — compile-time-checked SQL → typed Go. Best fit for the small, mostly-static schema. |
| Migrations | `golang-migrate` · `goose` · `atlas` | **`golang-migrate`** — plain `.sql` files, runs both PG and CH dialects. |
| Config | env vars + `caarlos0/env` · `viper` · `koanf` | **`caarlos0/env`** — small, struct-based, actively maintained. |
| Logging | `log/slog` (stdlib) · `zerolog` · `zap` | **`log/slog`** — stdlib, structured, plenty for this scope. |
| Sessions | signed cookies · PG-backed | **PG-backed `sessions` table** — easy to invalidate, observable. |
| Password hashing | `golang.org/x/crypto/bcrypt` | Default. |
| Testing | stdlib `testing` + `testify` for assertions | Default. |
| Static asset bundling | `embed.FS` in the binary | Decided — preserves the single-binary story. |

### Explicitly *not* using

- No separate worker process. No Sidekiq / Solid Queue equivalent.
- No MinIO / S3 fallback buffer in v1. (See "Ingest reliability" below.)
- No SPA, no Vite, no npm. The web UI is server-rendered HTML.

## Repo layout

```
/Dockerfile                 # production image — Kamal builds this
/cmd/server/main.go         # entry point — wires deps, starts HTTP server
/internal/
  /ingest/                  # batch buffer, validator, ClickHouse writer
  /query/                   # query executor: attaches additional_table_filters per request
  /events/                  # read-side: list events, sessions, persons, groups
  /mcp/                     # MCP endpoint + tool definitions
  /web/                     # web UI + OAuth handlers (login, projects, /oauth/*, /v1/whoami)
  /auth/                    # sessions, public ingest token, password hashing
  /oauth/                   # OAuth 2.1 server: codes, access tokens, PKCE, client registry
  /clickhouse/              # CH client (admin + readonly pools), schema bootstrap
  /postgres/                # PG client, sqlc-generated code
  /config/                  # env config struct
  /views/                   # templ files (.templ + generated .go)
  /static/                  # htmx.min.js, CSS — served via embed.FS
/migrations/
  /postgres/                # golang-migrate .sql files
  /clickhouse/              # golang-migrate .sql files (clickhouse dialect)
/config/
  deploy.yml                # Kamal config (committed; secrets via .kamal/secrets)
  deploy.example.yml        # template for self-hosters
  /deploy/
    /postgres/init.sql      # bootstrap: CREATE EXTENSION pgcrypto, etc.
    /clickhouse/users.xml   # defines mere_admin + mere_readonly users
    /clickhouse/config.xml  # async-insert defaults, log TTLs
/scripts/
  /operator/                # SQL scripts invoked by Kamal aliases (create-user.sql, reset-password.sql, wipe-project.sql, ...)
  dev                       # start docker compose + run server with hot reload
/docker/
  docker-compose.yml        # local dev only — Kamal handles production
/docs/
  plan.md                   # this file
  architecture.md           # written once the design stabilises
  api.md                    # public API reference
  self-host.md              # how to run it
/e2e/                       # blackbox HTTP tests against a running binary
```

`internal/` because nothing here is meant to be imported by other Go modules.

There is no operator CLI. Operator-only actions (password reset, project wipe, etc.) ship as **Kamal aliases** defined in `config/deploy.yml`, each invoking a SQL script in `/scripts/operator/` against the appropriate accessory (`postgres` or `clickhouse`). See "Operator actions" below.

## Data model

### Postgres (operational state)

| Table | Purpose |
|---|---|
| `users` | accounts (email + bcrypt password hash) |
| `teams` | a team owns one or more projects; every user has at least one (auto-created on signup) |
| `team_memberships` | user ↔ team; no role distinction in v1 (all members equal) |
| `projects` | scoped to a team; soft-deletable (`deleted_at` column) |
| `api_tokens` | scoped to a project; public ingest token (`mere_pub_…`) for `/ingest/v1/events` |
| `oauth_clients` | OAuth client registry (RFC 7591 dynamic registration) |
| `oauth_codes` | short-lived (10 min) PKCE authorization codes; one-shot |
| `oauth_access_tokens` | 1-hour bearer tokens for `/api/v1/* + /mcp`, scoped to (user, project) |
| `sessions` | web UI login sessions |
| `team_invites` | one-shot invite links: `token`, `team_id`, `created_by`, `expires_at`, `consumed_at` |
| `failed_events` | DLQ for ingest batches that failed to land in ClickHouse |

### ClickHouse (analytics)

Versioned tables (suffix `_vN`). The raw landing table is also the queryable surface; v1 deliberately does not add typed read models or derived endpoint-specific tables.

| Table | Type | Notes |
|---|---|---|
| `events_raw_v1` | MergeTree | Landing table. All ingest writes here; project-scoped query reads it through `mere_readonly` with tenant filters. |

All tables include `project_id` as a primary-key prefix component (cheap scoping at the part level).

### ClickHouse users

Exactly two, defined in `config/deploy/clickhouse/users.xml` and provisioned by the ClickHouse container on first start:

- **`mere_admin`** — full DDL/DML. Used for migrations and ingest writes.
- **`mere_readonly`** — `SELECT` only. Used by the query executor for all read traffic (`/api/v1/projects/:project_id/query`, `/api/v1/projects/:project_id/schema`, MCP read tools).

The ClickHouse image's built-in `default` user remains restricted to `127.0.0.1` (inside the container only) — used only by the operator via the `clickhouse-console` Kamal alias, never by the app.

Isolation between projects is enforced **at the application layer**, not at the database — see "Multi-tenant isolation" below.

### ClickHouse server settings (defaults worth shipping)

The `config/deploy/clickhouse/config.xml` ships with a `default` profile that turns on `async_insert`:

```xml
<async_insert>1</async_insert>
<wait_for_async_insert>0</wait_for_async_insert>
<async_insert_busy_timeout_ms>1000</async_insert_busy_timeout_ms>
<async_insert_max_data_size>10485760</async_insert_max_data_size>
```

This buffers small inserts ClickHouse-side and flushes in larger batches, reducing part count and write amplification. Same setting session_vision uses in production. We still batch app-side (see "Ingest reliability") — the two are complementary.

## Public API

The API has two planes. Public ingest lives under `/ingest/v1/*` and resolves the project from the public `mere_pub_` snippet token in `api_tokens`, since the snippet lives in client HTML. Private read/control endpoints live under `/api/v1/*`, use OAuth bearer tokens issued by the in-process OAuth 2.1 server at `/oauth/*`, and include the project ID in the URL.

**Soft-deleted projects return `404 Not Found` on every `/api/v1/projects/:project_id/*` endpoint** — the API behaves exactly as if the project never existed. Underlying ClickHouse data is retained until an operator runs `kamal wipe-project`, so a deletion can be undone via direct SQL up to that point. Public ingest tokens for soft-deleted projects return the same uniform auth failure as unknown/revoked tokens.

```
POST  /ingest/v1/events                    # batch events (1..N per request), project from public ingest token
POST  /api/v1/projects/:project_id/query   # SQL passthrough, project-scoped by URL + OAuth grant
GET   /api/v1/projects/:project_id/schema  # queryable table/column metadata for this project
GET   /mcp                                 # MCP endpoint — tools wrap query + schema
```

**Versioning policy:** `/v1` is **forever-stable**. Any breaking change ships at `/v2`. `/v1` and `/v2` coexist indefinitely. Additive changes (new fields, new endpoints) are allowed within `/v1`.

The query body is `{"sql": "..."}`. The response is `{"columns": [...], "rows": [...], "stats": {...}}`. ClickHouse errors are returned with the original message — power users want them. The `:project_id` path parameter is authoritative for tenant scoping, and the OAuth grant must authorize that exact project; mismatch returns `404`.

The schema response is a small JSON catalog of queryable tables and columns, e.g. `{"tables":[{"name":"events_raw_v1","columns":[...]}]}`. It is authenticated with the same OAuth bearer tokens as query and uses the same allowlist of queryable analytics tables.

## Ingest validation

Strict on required fields, lenient on extras:

- **Required**: `event` (string), `timestamp` (ISO 8601 or epoch ms). Project comes from the public ingest token (`mere_pub_…`), not the payload.
- **Optional but supported first-class**: `distinct_id`, `properties` (arbitrary JSON), `session_id`.
- **Extras**: any other top-level field is stored verbatim in a JSON column. No rejection. Consumers can query their own fields without us shipping a migration.
- **Rejection** = HTTP 400 with a per-event error array; the rest of the batch is accepted. We never silently drop.

## Web UI

Server-rendered (templ) + htmx for interactivity. Pages:

- `/login`, `/logout` — there is no public signup route. The first user is created by the operator via `kamal create-user` (see "Operator actions"); subsequent users join via invite links. This means the deployer doesn't need to front the URL with an ACL to keep strangers out.
- `/teams/:id` — team settings, member list, "Generate invite link" button. Clicking produces a one-shot URL (`/invites/:token`) that the inviter copies and shares out-of-band. Visiting the URL while logged-out renders an inline signup form (POST creates the account, consumes the invite, and logs the user in atomically); while logged-in, it adds the current user to the team. Each token is consumable once and has a 7-day TTL.
- `/teams/:id/projects` — projects in this team; create / soft-delete.
- `/projects/:id` — settings + the auto-provisioned public ingest token (always-visible copybox). `/api/v1/* + /mcp` bearers are not issued here; they come from the OAuth consent flow at `/oauth/authorize`.
- `/projects/:id/query` — SQL playground (textarea + Run button, results table, schema sidebar).

That's the whole UI. CodeMirror or Monaco gets dropped into the query page only if the textarea becomes a real bottleneck.

## Auth

- **Web UI:** session cookie (HttpOnly, SameSite=Lax), backed by `sessions` table.
- **`/ingest/v1/events`:** project-scoped public token (`mere_pub_…`) in `api_tokens`. Auto-provisioned at project create; non-secret by design (lives in client HTML). The project is resolved from the token, never from a URL path parameter.
- **`/api/v1/* + /mcp`:** OAuth 2.1 access tokens (PKCE-only authorization-code flow). Stored hashed (sha256) in `oauth_access_tokens`; 1-hour TTL; one project per grant chosen at consent. Server lives in-process at `/oauth/{register,authorize,token}` with RFC 8414 discovery at `/.well-known/oauth-authorization-server`. No refresh tokens — re-authorize on expiry. Implementation in `internal/oauth/`.
- **No in-app password reset in v1.** Operators reset via the `kamal reset-password` alias (see "Operator actions"), which executes a SQL `UPDATE` against the `postgres` accessory using `pgcrypto`'s `crypt(..., gen_salt('bf', 10))`. Go's `bcrypt` and `pgcrypto`'s `bf` hashes are wire-compatible (both produce standard `$2a$` format), so the user logs in normally afterwards. The user is forced to change the password on next login.

## Multi-tenant isolation

Application-layer, not database-layer. The mechanism is ClickHouse's **`additional_table_filters`** session setting: the executor attaches a filter to every queryable analytics table for the duration of the query, and ClickHouse transparently applies it to every reference to those tables.

Implementation:

```go
ctx := clickhouse.Context(r.Context(), clickhouse.WithSettings(map[string]any{
    "additional_table_filters": fmt.Sprintf(
        "{'analytics.events_raw_v1': 'project_id = ''%s'''}",
        projectID,
    ),
}))
rows, err := readonlyPool.QueryContext(ctx, userSQL)
```

The user's SQL is sent through **unmodified** — no parsing, no CTE wrapping, no substring rewriting. ClickHouse merges the filter into the query plan at execution time. The connection runs as `mere_readonly`.

Why this over CTE rewriting:
- No SQL parser to maintain or audit — fewer escape hatches for an attacker to find.
- The filter applies to every reference to the table, including references inside views and joins the user might write.
- Native to ClickHouse; the implementation is a single map literal, not a SQL transformer.

Sanity-check tests for step 6:
- Naive query: `SELECT count() FROM analytics.events_raw_v1` → only this project's rows.
- Self-join: `SELECT * FROM events_raw_v1 a JOIN events_raw_v1 b ON a.distinct_id = b.distinct_id` → both references filtered.
- Aliases / `FROM (SELECT ...)` subqueries → still filtered.
- User attempts to bypass via `SETTINGS additional_table_filters = {...}` in their own SQL → ClickHouse merges; our filter still applies (verify).
- Tables not in the map (e.g. `system.numbers`) → not filtered, but `mere_readonly` can't reach them anyway.

## Ingest reliability

Each `POST /ingest/v1/events` request:

1. Parses + validates the batch (per "Ingest validation" above).
2. Pushes events onto an in-memory channel.
3. Returns `202 Accepted` immediately.

A background flusher (goroutine, started at boot) drains the channel:

- Flushes when the buffer hits N events or T seconds, whichever comes first.
- Writes a single batched INSERT to `events_raw_v1` as `mere_admin`.
- On failure: writes the batch to the `failed_events` table in Postgres.

A second goroutine periodically drains `failed_events` back into ClickHouse. On successful drain the row is deleted — the table is purely a retry buffer, not an audit trail.

Tradeoffs vs. the session_vision design:
- **No MinIO/S3** in v1 — Postgres DLQ handles short outages. If ClickHouse is down for hours, rows accumulate in Postgres; that's the operator's signal to investigate.
- **In-process** instead of Solid Queue worker — simpler to deploy. If we need horizontal scale later, this becomes a worker process reading off PG or a real queue.

If the in-memory channel saturates (CH slow + DLQ flusher behind), we respond `503` with `Retry-After`. Callers retry; we never silently drop.

## Operator actions

Anything that can't be done through the web UI or HTTP API is done via a **Kamal alias**, defined in `config/deploy.yml`. The alias `kamal exec`s into the appropriate accessory and runs a parameterised SQL script from `/scripts/operator/` (baked into the postgres/clickhouse container images at build, or mounted as a volume).

Example `deploy.yml` entries:

```yaml
aliases:
  create-user: >
    accessory exec postgres -i
    "psql -U $POSTGRES_USER -d $POSTGRES_DB
     -v email=$EMAIL -v password=$INITIAL_PASSWORD
     -f /operator/create-user.sql"

  reset-password: >
    accessory exec postgres -i
    "psql -U $POSTGRES_USER -d $POSTGRES_DB
     -v email=$EMAIL -v password=$NEW_PASSWORD
     -f /operator/reset-password.sql"

  wipe-project: >
    accessory exec clickhouse -i
    "clickhouse-client --query
     \"ALTER TABLE analytics.events_raw_v1 DELETE WHERE project_id = '$PROJECT_ID'\""
```

Operator invocation from their laptop (after `kamal config` is set up):

```bash
EMAIL=admin@example.com INITIAL_PASSWORD=temp-pw-1234 kamal create-user
EMAIL=user@example.com  NEW_PASSWORD=temp-pw-1234     kamal reset-password
PROJECT_ID=01HX... kamal wipe-project
```

Initial v1 alias set:

| Alias | Purpose | Backend |
|---|---|---|
| `create-user` | Bootstrap a user (with auto-named personal team and membership) on a fresh deploy. The first user is created this way; subsequent users come in via invite links. `must_change_password=true` is set so they're forced to rotate on first login. | `postgres` + `pgcrypto` |
| `reset-password` | Force-reset a user's password | `postgres` + `pgcrypto` |
| `wipe-project` | Permanently delete a project's CH data after soft-delete | `clickhouse` |
| `db-console` | Open `psql` against the running deployment | `postgres` |
| `clickhouse-console` | Open `clickhouse-client` against the running deployment | `clickhouse` |

Requires `CREATE EXTENSION pgcrypto;` in the Postgres bootstrap migration. Standard, ships with PG.

The operator's privilege is exactly equal to "has `kamal` config + SSH to the deployment" — same level of trust we already assume for whoever can `kamal deploy`. No new escalation surface.

## Deploy

The promise: a fresh Hetzner VPS → `kamal setup` → working analytics deployment with TLS, PostgreSQL, ClickHouse, and the app, in one command. No Terraform, no Ansible, no manual `apt install`. Same pattern session_vision uses today.

### What the operator does (from zero)

```bash
# 1. Provision a Hetzner VPS, get root SSH access.
# 2. Clone the repo locally:
git clone https://github.com/<org>/mere && cd mere

# 3. Copy the example deploy config, fill in three things:
cp config/deploy.example.yml config/deploy.yml
# edit:
#   - servers.web.hosts (your VPS IP)
#   - proxy.host (your hostname for TLS)
#   - registry.username (your GHCR / DockerHub username)

# 4. Create .kamal/secrets — Kamal reads these into env at deploy time.
#    The ClickHouse users.xml expects SHA-256 hashes (not plaintext), so we
#    precompute them here and reference the *_SHA256 vars from the templated XML.
mkdir -p .kamal && cat > .kamal/secrets <<'EOF'
KAMAL_REGISTRY_PASSWORD=$(op read "op://Personal/GHCR/token")
POSTGRES_PASSWORD=$(openssl rand -hex 32)
CLICKHOUSE_ADMIN_PASSWORD=$(openssl rand -hex 32)
CLICKHOUSE_ADMIN_PASSWORD_SHA256=$(echo -n "$CLICKHOUSE_ADMIN_PASSWORD" | sha256sum | awk '{print $1}')
CLICKHOUSE_READONLY_PASSWORD=$(openssl rand -hex 32)
CLICKHOUSE_READONLY_PASSWORD_SHA256=$(echo -n "$CLICKHOUSE_READONLY_PASSWORD" | sha256sum | awk '{print $1}')
SESSION_SECRET=$(openssl rand -hex 64)
EOF

# 5. One command does everything.
kamal setup

# 6. Bootstrap the first user. They'll be forced to change the password
#    on first login.
EMAIL=admin@example.com INITIAL_PASSWORD=temp-pw-1234 kamal create-user
```

`kamal setup` runs end-to-end on the VPS:

1. SSHes in as root, installs Docker.
2. Pulls `postgres:16` and starts it as an accessory with `config/deploy/postgres/init.sql` mounted at `/docker-entrypoint-initdb.d/init.sql` (creates the database + `CREATE EXTENSION pgcrypto`).
3. Pulls `clickhouse/clickhouse-server:24.12` and starts it with `config/deploy/clickhouse/users.xml` mounted — provisions `mere_admin` + `mere_readonly` users with SHA-256 hashes of the passwords from `.kamal/secrets`.
4. Builds the Go image from the repo's `Dockerfile`, pushes to the configured registry.
5. Pulls the image on the VPS, starts the app container.
6. App entry point runs pending PG migrations (as the app's PG user) and CH migrations (as `mere_admin`), then starts the HTTP server.
7. `kamal-proxy` fronts the app, terminates TLS via Let's Encrypt for the configured hostname.

After `kamal setup`, the operator runs `kamal create-user` once to create the first account; further users come in through invite links generated inside the web UI. Subsequent releases are just `kamal deploy`: pulls the new image, restarts the container (which re-runs pending migrations on boot), zero-downtime rollout.

### `config/deploy.yml` shape

```yaml
service: mere
image: <registry-username>/mere

servers:
  web:
    hosts:
      - <VPS_IP>            # filled in by operator

registry:
  username: <GHCR_USERNAME> # filled in by operator
  password:
    - KAMAL_REGISTRY_PASSWORD

proxy:
  ssl: true
  host: <HOSTNAME>          # filled in by operator
  app_port: 8080
  healthcheck:
    path: /healthz
    interval: 3

env:
  clear:
    PORT: 8080
    POSTGRES_HOST: mere-postgres
    POSTGRES_PORT: 5432
    POSTGRES_DB: mere
    POSTGRES_USER: mere
    CLICKHOUSE_HOST: mere-clickhouse
    CLICKHOUSE_PORT: 9000
    CLICKHOUSE_DATABASE: analytics
    CLICKHOUSE_ADMIN_USER: mere_admin
    CLICKHOUSE_READONLY_USER: mere_readonly
  secret:
    - POSTGRES_PASSWORD
    - CLICKHOUSE_ADMIN_PASSWORD
    - CLICKHOUSE_READONLY_PASSWORD
    - SESSION_SECRET

accessories:
  postgres:
    image: postgres:16
    host: <VPS_IP>
    port: "127.0.0.1:5432:5432"
    env:
      clear:
        POSTGRES_DB: mere
        POSTGRES_USER: mere
      secret:
        - POSTGRES_PASSWORD
    directories:
      - /data/mere/postgres:/var/lib/postgresql/data
    files:
      - config/deploy/postgres/init.sql:/docker-entrypoint-initdb.d/init.sql

  clickhouse:
    image: clickhouse/clickhouse-server:24.12
    host: <VPS_IP>
    port: "127.0.0.1:8123:8123"
    env:
      clear:
        CLICKHOUSE_DB: analytics
        CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT: 1
      secret:
        - CLICKHOUSE_ADMIN_PASSWORD
        - CLICKHOUSE_READONLY_PASSWORD
    directories:
      - /data/mere/clickhouse/data:/var/lib/clickhouse
      - /data/mere/clickhouse/logs:/var/log/clickhouse-server
    files:
      - config/deploy/clickhouse/users.xml:/etc/clickhouse-server/users.d/users.xml
      - config/deploy/clickhouse/config.xml:/etc/clickhouse-server/config.d/config.xml
    options:
      ulimit: "nofile=262144:262144"

aliases:
  create-user:       accessory exec postgres -i "psql -U $POSTGRES_USER -d $POSTGRES_DB -v email=$EMAIL -v password=$INITIAL_PASSWORD -f /operator/create-user.sql"
  reset-password:    accessory exec postgres -i "psql -U $POSTGRES_USER -d $POSTGRES_DB -v email=$EMAIL -v password=$NEW_PASSWORD -f /operator/reset-password.sql"
  wipe-project:      accessory exec clickhouse -i "clickhouse-client --query \"ALTER TABLE analytics.events_raw_v1 DELETE WHERE project_id = '$PROJECT_ID'\""
  db-console:        accessory exec postgres -i "psql -U $POSTGRES_USER -d $POSTGRES_DB"
  clickhouse-console: accessory exec clickhouse -i "clickhouse-client"
  console:           app exec --interactive --reuse "sh"
  logs:              app logs -f
```

### Persistence

PG and CH data live in named host volumes under `/data/mere/` on the VPS. They survive `kamal redeploy`, `kamal app remove`, and image upgrades. They are lost only if the operator explicitly deletes the directory or the VPS itself.

### Dockerfile contract

Multi-stage build:

1. `golang:1.25-alpine` builder stage — runs `go mod download`, `templ generate`, `go build -ldflags='-s -w'` → produces a static `mere-server` binary.
2. `alpine:3.19` (or `scratch` if cgo isn't needed) runtime stage — copies the binary, copies `/migrations/` so the entrypoint can find them.
3. ENTRYPOINT: `/mere-server` — the binary itself reads env, runs migrations, starts serving. No separate entrypoint script.

Result: ~20-30MB image. Reproducible. Pinned base image tags.

### Migration auto-run

On every container start (so both `kamal setup` and `kamal deploy`), the binary:

1. Connects to Postgres as the app user; runs pending migrations from the embedded `migrations/postgres/` FS.
2. Connects to ClickHouse as `mere_admin`; runs pending migrations from the embedded `migrations/clickhouse/` FS.
3. Starts the HTTP server.

Migrations are append-only (per the immutable-migration rule from the rebrand docs). A failed migration aborts startup — the container exits, `kamal-proxy` keeps routing to the previous version, and the operator sees the error in `kamal logs`.

### `config/deploy/postgres/init.sql`

```sql
-- Bootstraps the postgres accessory on first startup.
-- Runs once, when the data directory is empty.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
-- Database + user are created by the postgres image from POSTGRES_DB / POSTGRES_USER env vars,
-- so we don't need to CREATE DATABASE here.
```

### `config/deploy/clickhouse/users.xml`

Templated. The repo ships a `users.xml` file with `${CLICKHOUSE_ADMIN_PASSWORD_SHA256}` / `${CLICKHOUSE_READONLY_PASSWORD_SHA256}` placeholders. Kamal renders the file before uploading to the VPS (via the `files:` directive plus a small `envsubst`-style pre-deploy hook, since stock Kamal doesn't template `files:` directly).

The SHA-256 values themselves live in `.kamal/secrets`, precomputed alongside the plaintext password (see the bootstrap snippet above). The plaintext never reaches the ClickHouse server — only the hashes.

Structure mirrors session_vision: `default` user restricted to `127.0.0.1`, `mere_admin` (full access) and `mere_readonly` (read-only profile) accept connections from the Docker network.

### docker-compose for local dev

`/docker/docker-compose.yml` exists for laptop development only: brings up `postgres` + `clickhouse` so `go run ./cmd/server` works. Not used in production. Kamal is the only production path.

### Image registry

Published images go to **GHCR** (`ghcr.io/<org>/mere:vX.Y.Z`), public, no auth for pull. CI publishes **amd64-only** on every git tag in v1; arm64 added later if anyone asks. The deploy.yml template defaults to GHCR; self-hosters can override to push from their own machine to their own registry for air-gapped deploys.

### Backups

**The recommended path is Hetzner's built-in automated backups** — enable it on the VPS in the Hetzner Cloud Console (Server → Backups, ~20% of the VPS cost). It takes daily disk-level snapshots and keeps the last 7. Because `/data/mere/postgres` and `/data/mere/clickhouse` are on the same VPS disk, the snapshot covers both databases consistently.

Self-hosters on other providers, or those who want logical (not block-level) backups, run `pg_dump` and `clickhouse-backup` themselves — `docs/self-host.md` includes example scripts but no built-in accessory. We can add a backup accessory in v2 if it becomes a common ask.

## Build order

Each step leaves a runnable, useful binary. The topic sections above ([Architecture](#architecture), [Data model](#data-model), [Public API](#public-api), [Ingest validation](#ingest-validation), [Web UI](#web-ui), [Auth](#auth), [Multi-tenant isolation](#multi-tenant-isolation), [Ingest reliability](#ingest-reliability), [Operator actions](#operator-actions), [Deploy](#deploy)) remain authoritative for cross-cutting design; each step below references them and adds step-specific decisions, error/rescue notes, and tests.

### Step 1 — Skeleton [DONE]

Shipped in PR #2. `main.go` boots; config loaded; slog initialized; `/healthz` returns "ok"; templ "hello" page at `/`; multi-stage Dockerfile; embedded migrations FS; e2e boot test.

### Step 2 — Databases [DONE]

Shipped in PR #2. PG + CH connection pools (admin + readonly); idempotent `CREATE DATABASE` / `CREATE USER` / `GRANT` for readonly; golang-migrate runner with dirty-state operator runbook; initial PG schema (`users`, `teams`, `team_memberships`, `projects`, `api_tokens`, `sessions`) and CH schema (`events_raw_v1`).

### Step 3 — Auth [DONE]

Shipped on branch `jjdinho/review-plan-next-step`. A user can sign up, log in, and log out via the web UI; the operator `reset-password.sql` works against the dev stack and is covered by integration tests.

**What landed (vs. the original spec below):**

- `sqlc` adopted as planned. `sqlc.yaml` at repo root; `internal/postgres/queries/*.sql` source; generated code in `internal/postgres/db/`. UUIDs kept as `string` (matches `idgen.New()`).
- New PG migrations: `0002_sessions_csrf` (adds `csrf_token`), `0003_pgcrypto` (extension for the operator script).
- `internal/auth/` — `password.go` (bcrypt cost 10, `ValidationError`, email normalize/validate), `csrf.go` (32-byte base64url tokens, constant-time compare), `context.go` (`Session` + CSRF context helpers), `service.go` (atomic Signup tx; Authenticate; session create/lookup/touch/destroy with 7-day sliding window, 30-day hard cap).
- `internal/web/auth_middleware.go` — session-cookie middleware (sliding-expiry touch, refresh on each request), anonymous `mere_csrf` cookie fallback for pre-auth forms, CSRF enforcement that exempts API routes and `/mcp`.
- `internal/web/handlers.go` — `/signup`, `/login`, `/logout` handlers.
- `internal/web/server.go` refactored to `web.Handler(Options{AuthService, Logger, SecureCookies})`.
- Views: `layout.templ` (`@CSRFField()` helper + global `hx-headers`), `signup.templ`, `login.templ`, `home.templ`, refreshed `index.templ`, plus `internal/static/app.css`.
- `config.SecureCookies` env (`SECURE_COOKIES`, default `true`); `scripts/dev` sets it to `false` for plaintext localhost.
- `scripts/operator/reset-password.sql` — bcrypt cost 10 via pgcrypto; sets `must_change_password=true`; raises on unknown email (non-zero exit). Implementation note: `psql ':var'` substitution does not fire inside `$$`-quoted `DO` blocks, so parameters are stashed via `set_config(..., true)` and recovered with `current_setting(...)`.

**Deferred to a later step (with reason):**

- `internal/auth/viewer.go` — explicitly scoped to step 4 ("Teams + projects + tokens") in the spec; nothing in step 3 needs it.
- `must_change_password` enforcement at login — column exists and the operator script sets it; gating login behind a forced password-change page is part of the post-reset UX and was not required by the step 3 "done when" line. Capture as a step-4-or-later UX item.
- `must_change_password` flag is wired through `auth.Session` but not yet displayed in the UI.

**Tests delivered (all green):**

- Unit (`internal/auth/`): bcrypt round-trip, length/format validation, email normalization, CSRF token shape + equality (incl. empty fail-closed), sliding-window/hard-cap math at boundaries.
- Integration (testcontainers Postgres, `internal/auth/`): Signup atomicity across user+team+membership; duplicate-email → `ErrEmailTaken` (case-insensitive); short password → `ValidationError`; Authenticate ok/invalid (both wrong-password and unknown-email collapsed to `ErrInvalidCredentials`); session create/lookup/destroy; expired session returns `ErrSessionExpired` and is opportunistically deleted; touch extends within cap.
- HTTP integration (`internal/web/auth_integration_test.go`): signup → home renders email; login sets cookie; logout destroys it; invalid creds → 401 + no cookie; missing CSRF token → 403; wrong CSRF token → 403; duplicate-email signup → 409; templ escapes attacker-controlled email (`<img src=x>@...` rendered as `&lt;img src=x&gt;`).
- Operator (`internal/auth/reset_password_test.go`): unknown email → non-zero exit + "no user with email" stderr; known email → user can log in with new password and not the old one; lookup is case-insensitive.

---

**Original spec for reference:**

**Goal:** A user can sign up, log in, log out via the web UI. An operator can reset a locked-out user's password via `kamal reset-password`.

**Implement per:** [Auth](#auth) section.

**Schema additions:**
- New PG migration: add `csrf_token TEXT NOT NULL` to `sessions`.

**New code:**
- `/internal/auth/` — password hashing (bcrypt cost=10), session create/lookup/destroy, CSRF token generation.
- `/internal/web/` handlers: `/signup`, `/login`, `/logout`.
- `/internal/web/middleware.go`: session-cookie middleware, CSRF middleware.
- `/internal/views/`: `signup.templ`, `login.templ`, `layout.templ` with `@csrfField()` helper + `hx-headers` script.
- `/scripts/operator/reset-password.sql` — `UPDATE` wrapped in `DO` block that `RAISE EXCEPTION` on `NOT FOUND`.

**Decisions for this step:**
- Session cookie: `mere_session`, HttpOnly, SameSite=Lax, PG-backed. Sliding expiry on each authenticated request; hard cap 30 days.
- CSRF: per-session token in `sessions.csrf_token`; templ `@csrfField()` helper for forms; htmx layout sets `hx-headers='{"X-CSRF-Token":"..."}'` globally; middleware verifies on non-GET requests to web routes; API routes and `/mcp` exempt (Bearer/public-token auth, no cookie).
- No public signup. The first user is created by the operator via `kamal create-user` (SQL script against the postgres accessory, same trust boundary as `kamal reset-password`); subsequent users join via invite links. Decisions log #4.
- Bcrypt cost = 10 (Go default).
- `reset-password.sql` raises on unknown email so kamal alias exits non-zero (don't silently report success on a typo'd email).
- Adopt `sqlc` here, before any hand-written PG queries: set up `sqlc.yaml`, generate into `internal/postgres/db/`, use generated queries from step 3 onward.

**Error/rescue:**
- Duplicate email on signup → re-render form with "email already registered".
- Invalid credentials on login → re-render with "invalid credentials" (do not distinguish user-not-found vs wrong-password).
- Session lookup ctx timeout (200ms) → treat as unauthenticated, redirect to `/login`.
- CSRF mismatch on non-GET → 403.

**Tests:**
- Unit: bcrypt round-trip; CSRF token gen; session expiry math.
- Integration: signup creates user+team+membership in one transaction; login sets cookie; logout deletes session; CSRF rejection (no token, wrong token, expired session); regression test that templ escapes `<script>` in usernames.
- Operator: `reset-password.sql` exits non-zero on unknown email.

**Done when:** A user can sign up, log in, see a logged-in page, log out. `reset-password` kamal alias works against the local docker-compose stack.

### Step 4 — Teams + projects + tokens [DONE]

Shipped on branch `jjdinho/check-build-progress`. A logged-in user can view their teams, create projects, issue/revoke API tokens, generate invite links, and join teams via the invite-confirm flow. The forced-password-change interstitial is wired into the same `authMiddleware`.

**What landed (vs. the original spec below):**

- `internal/auth/viewer.go` — per-request `*Viewer` attached to ctx by `authMiddleware`; fluent chains `Teams(ctx).ByID(id) / List() / MembersOf(id)`, `Projects(ctx).ByID(id) / ListForTeam / ListForTeams / Create / SoftDelete`, `Tokens(ctx).ListForProject / Create / Revoke`, plus `Viewer.CreateInvite(ctx, teamID, now)`. Membership miss returns typed **`auth.ErrNotVisible`** (not `pgx.ErrNoRows` — handlers `errors.Is → 404`).
- `internal/auth/tokens.go` — `GenerateToken()` → `"mere_pat_" + base64.RawURLEncoding(32 bytes)` = exactly 52 chars; `HashToken` (sha256 hex); `LooksLikeAPIToken` for the future bearer middleware (Step 5).
- `internal/auth/name.go` — generic `ValidateName(label, value)` (trim + non-empty + ≤100), used by team/project/token forms.
- `internal/auth/service.go` extended — `ConsumeInvite(ctx, userID, plaintext)` atomic tx (UPDATE invite + ON CONFLICT DO NOTHING membership + GetTeamByID, one tx); `SignupWithInvite(ctx, req, invitePlaintext)` strict path (invalid invite aborts the whole signup tx); `ChangePassword`; `Queries()` accessor for middleware viewer construction; new `team_memberships_pkey` tolerance via the new `CreateTeamMembershipIfMissing` query.
- `internal/web/auth_middleware.go` — attaches `*Viewer` to ctx after session; **`must_change_password` gate**: flagged sessions redirect to `/account/password` from every route except the whitelist (`/account/password`, `/logout`, `/static/*`).
- New handlers: `internal/web/teams_handlers.go`, `projects_handlers.go`, `invites_handlers.go`, `account_handlers.go`, plus `util.go` (`absoluteURL`).
- Routes registered in `internal/web/server.go`: `/teams/{id}`, `/teams/{id}/invites`, `/teams/{id}/projects`, `/projects/{id}`, `/projects/{id}/delete`, `/projects/{id}/tokens`, `/projects/{id}/tokens/{tid}/revoke`, `/invites/{token}` (GET public + POST authed), `/account/password` (GET+POST). Signup + login extended for `?invite=:t` carryover.
- Views: `home.templ` rebuilt as flat teams×projects landing; new `team_show.templ`, `project_show.templ`, `invite_confirm.templ`, `invite_invalid` page, `account_password.templ` (with explicit "Your password was reset by an operator" banner driven by `session.MustChangePassword`). `signup.templ` / `login.templ` extended for invite carryover.
- New PG migration `0004_team_invites` — table + partial unique index `team_invites_token_hash_active_idx WHERE consumed_at IS NULL` (mirrors `api_tokens` pattern + free collision insurance) + listing index.
- New sqlc query files: `projects.sql`, `api_tokens.sql`, `team_invites.sql`; `teams.sql` extended with `GetTeamForUser`, `ListMembersForTeamForUser`, `IsMemberOfTeam`, `CreateTeamMembershipIfMissing`. All membership-gated via `JOIN team_memberships` or `WHERE EXISTS`; soft-deleted projects filtered everywhere.

**Decisions made during plan-eng-review (deviations from the original spec):**

- Package layout: **no `/internal/projects/` package**. Project/token CRUD goes through the viewer; transactional ops (invite consume, signup-with-invite) live on `auth.Service`. Avoids a passthrough abstraction over sqlc.
- Viewer construction: **per-request `*Viewer` in ctx**, built by `authMiddleware`. Callers use `auth.ViewerFrom(ctx).Projects(ctx).ByID(id)`. Not the literal `viewer.Projects(ctx)` package-level free chain from the original spec — would have required the queries pool in ctx (invisible magic).
- Viewer error contract: **typed `auth.ErrNotVisible`** instead of bare `pgx.ErrNoRows` (single sentinel for membership-miss; preserves room to log/metric "not visible" vs. "doesn't exist" if needed later).
- Token plaintext display: **render-on-POST** (no PRG, no cookie/DB flash). The "exactly once" semantic maps literally to "one response, then it's gone." Skipped the `hx-disable-elt` double-click affordance — render-on-POST already makes a refresh inert.
- Invite consume HTTP shape: **GET-confirm + POST-consume** (CSRF-protected). Blocks the cross-site auto-join attack that a GET-mutates-state design would enable.
- Anonymous-invite carryover: **query-param chain** (`/invites/:t → /signup?invite=:t` with hidden field). No cookie magic, no stale-cookie ghosts.
- Already-a-member invite consume: **silent success, invite burned, banner**. Matches user mental model.
- Signup-with-invalid-invite at POST: **strict** — signup fails with form error, user re-submits without invite. Preserves "intent honored or not at all" semantics.
- Home page IA: **flat home** at `/` (teams × projects), bounded 2-query (`ListTeamsForUser` + `ListProjectsForTeams(=ANY(ids))`) to eliminate N+1 footgun.
- `must_change_password` gate: **inside `authMiddleware`** (one middleware decides session + viewer + CSRF + freshness), not a separate `requireFreshPassword` wrap.
- Bearer-auth lookup: **deferred to Step 5** entirely. No `LookupTokenForBearer` helper in Step 4.

**Tests delivered (all green, `-race` clean on the concurrency tests):**

- Unit (`internal/auth/`): `tokens_test.go` (format `len == 52`, prefix, base64url alphabet, hash determinism, plaintext uniqueness, `LooksLikeAPIToken` table); `name_test.go` (trim, empty/whitespace-only/over-max, label embedding).
- Integration (testcontainers PG, `internal/auth/`): viewer cross-user (Alice's team is `ErrNotVisible` to Bob); project soft-delete hides from owner and idempotent on second call; token plaintext never echoed in `ListForProject`; revoke idempotent; cross-user revoke `ErrNotVisible`; bounded-2-query `ListForTeams` (empty input no-op).
- Invite integration (`internal/auth/invite_test.go`): happy path; invalid token → `ErrInviteInvalid`; already-consumed → `ErrInviteInvalid`; **already-member → silent success + invite burned**; **concurrent goroutine race** (two distinct users, `chan struct{}` start gate, asserts exactly one winner); signup-with-invalid-invite → tx rollback (no user lands); signup-with-valid-invite → user in two teams + invite consumed; `ChangePassword` happy + wrong-current + short-new + old-password-no-longer-works.
- HTTP integration (`internal/web/step4_integration_test.go`): home lists teams + projects, creating a project shows up on next render; project token create returns plaintext exactly once + subsequent GET to project page does NOT contain the plaintext or even the `mere_pat_` prefix (negative leakage assertion); soft-delete then GET → 404; **cross-user authorization matrix** (Alice tries 7 routes against Bob's UUIDs, all return 404 — parameterized subtests, new routes enroll here); anon `/invites/:t` → signup CTA → /signup?invite=:t hidden field → submit → user lands with 2 teams; strict-on-invalid-invite (400 + form message); CSRF on invite POST returns 403; **`must_change_password` gate** — flagged session redirects `/` → `/account/password`, `/account/password` reachable with "reset by an operator" banner, successful change clears flag and home is reachable again; invalid invite token renders the "no longer valid" page with 404.
- Schema (`internal/postgres/postgres_test.go`): updated table set + added `team_invites_token_hash_active_idx` and `team_invites_team_id_idx` to the index assertion.

**Bugs caught + fixed during implementation:**

- PG `25P02` (current transaction is aborted) inside `ConsumeInvite` when the `team_memberships` insert hit `team_memberships_pkey` for an already-member caller. Fix: new sqlc query `CreateTeamMembershipIfMissing` using `INSERT ... ON CONFLICT (team_id, user_id) DO NOTHING`. Applied to both `ConsumeInvite` and `SignupWithInvite` for consistency.
- Templ parser refused string concatenation inside `@Layout("Join "+data.TeamName, session)` — moved the title into the data struct as a precomputed `PageTitle` field.

**Deferred to a later step (with reason):**

- Member removal from team (TODOS.md item) — closes the membership-lifecycle loop but no self-hoster has hit it yet.
- API token expiry (TODOS.md item) — tokens are forever-valid until manually revoked; standard hygiene but not v1-blocking.
- Revoked-tokens audit view (TODOS.md item) — needs Step 5's bearer middleware to add `last_used_at` for the view to carry real signal.
- Bearer-auth lookup function — owned by Step 5 (its first consumer is public ingest).

**Subsequent change (OAuth refactor):** Step 4's `mere_pat_…` secret-API bearer (and the render-on-POST issuance UX) was retired in favor of an OAuth 2.1 authorization-code + PKCE flow. `api_tokens` now only carries the public `mere_pub_…` snippet token; private API + `/mcp` bearer auth goes through `oauth_access_tokens`. The historical record above is preserved verbatim; the live design is in `internal/oauth/` and migrations `0006_oauth` + `0007_drop_api_token_kind`.

---

**Original spec for reference:**

**Goal:** A logged-in user can create teams, invite members, create projects, issue / revoke API tokens.

**Implement per:** [Auth](#auth) (token format) + [Web UI](#web-ui).

**Schema additions:**
- New PG migration: `team_invites` (`id, token_hash, team_id, created_by, expires_at, consumed_at`); token stored as sha256 hash like `api_tokens`.

**New code:**
- `/internal/projects/` — project CRUD with soft-delete.
- `/internal/auth/tokens.go` — generation, hashing, lookup.
- `/internal/auth/viewer.go` — `viewer.Projects(ctx).ByID(id)` / `viewer.Teams(ctx).ByID(id)` helpers; carry `user_id` from session middleware; JOIN `team_memberships`; missing membership → `sql.ErrNoRows`.
- `/internal/web/` handlers: `/teams/:id`, `/teams/:id/projects`, `/projects/:id`, `/projects/:id/tokens`, `/invites/:token`.
- `/internal/views/`: team, projects, project-detail, token-create (visible-once UX), invite-consume.

**Decisions for this step:**
- Bearer token format: `mere_pat_<43-char base64url of 32 random bytes>`. Stored as sha256 hex in `api_tokens.token_hash`. Visible **exactly once** at creation in the web UI: warning banner + copy-to-clipboard button + "I've saved the token" explicit dismissal. Prefix enables leak scanners (TruffleHog, GitHub secret scanning).
- All web UI data access goes through `viewer.X(ctx).ByID(id)`; missing membership = **404** not 403 (avoid confirming existence; protects against UUID enumeration).
- Team invite consume: atomic `UPDATE team_invites SET consumed_at = NOW() WHERE token_hash = $1 AND consumed_at IS NULL AND expires_at > NOW() RETURNING ...`; check `RowsAffected == 1`. 7-day TTL.
- Project soft-delete sets `deleted_at`; CH data persists. No UI restore (operator-recoverable via `UPDATE projects SET deleted_at = NULL`).
- Bearer auth lookup against soft-deleted project: 404 on private project-scoped API endpoints; token itself isn't revoked, just inaccessible.

**Error/rescue:**
- Token "Create" double-click → `hx-disable-elt='this'` on every htmx button (templ helper).
- Direct object reference for someone else's project → viewer returns no rows → 404.
- Invite already-consumed / expired → 404 (don't distinguish).

**Tests:**
- Unit: token format/length, sha256 round-trip, invite expiry math.
- Integration: token-create flow returns plaintext exactly once; subsequent loads never expose plaintext; project soft-delete makes private project-scoped API endpoints return 404; invite consume-once race (two concurrent `UPDATE` → only one wins).
- Security: cross-user authorization on every `/teams/:id` and `/projects/:id` route — user A cannot view user B's data.

**Done when:** Create a team via UI, invite a second user, create a project, issue a token (visible once), revoke it, soft-delete the project.

#### Post-step-4 follow-up: operator-bootstrapped first user [DONE]

Shipped after step 4. Removes the public `/signup` route; the first user is created by the operator via a new `kamal create-user` alias, and logged-out invitees now create their account inline on `/invites/:token` instead of being routed to `/signup?invite=:t`. Motivation: the prior model ("open signup; deployer fronts the URL with an ACL") was awkward because the same hostname also serves public ingest, which must stay reachable; path-based proxy rules are non-trivial and undercut the "easily deployed" promise. The new flow needs no proxy ACL — strangers can't create accounts because there's no public path to do so. See [decisions log #4](#decisions-log).

- New: `scripts/operator/create-user.sql` (mirrors `reset-password.sql` conventions: pgcrypto bcrypt cost 10, GUC stashing for `:vars` inside `DO` blocks, `must_change_password=TRUE`, descriptive duplicate-email error). IDs are `gen_random_uuid()` (v4) — deliberate exception to the app's UUID v7 convention; no codepath orders by `id`.
- Removed: `/signup` route, `getSignup`/`postSignup` handlers, `signup.templ`. `auth.Service.Signup` is retained as a test/seed primitive only (doc-commented as non-HTTP).
- Tightened: `auth.Service.SignupWithInvite` requires a non-empty invite token; `SignupResult` gains `InvitedTeam` so the anon-invite handler can redirect to the invited team page after auto-login.
- `/invites/:token` GET renders an inline `email + password` form for anon viewers (plus an "Already have an account? Log in" link to `/login?invite=:t` for existing users). POST creates the account, consumes the invite, and sets a session cookie in one transaction.
- Tests: `internal/auth/create_user_test.go` (happy path, duplicate email, short password, empty email); `internal/web/*` rewritten to seed via `svc.Signup` directly + new tests for the anon-invite POST (happy, duplicate email, invalid token, CSRF).
- Plan alias entry + "What the operator does (from zero)" updated; decisions log #4 + #15 updated.

### Step 5 — Ingest

**Goal:** `curl POST /ingest/v1/events` lands events in `events_raw_v1`. Survives transient CH outages via DLQ; surfaces both-down state loudly.

**Implement per:** [Ingest validation](#ingest-validation) + [Ingest reliability](#ingest-reliability).

**Schema additions:**
- New PG migration: `failed_events` — `(id, batch_payload JSONB, last_error TEXT, attempt_count INT NOT NULL DEFAULT 0, created_at, last_attempt_at, quarantined_at TIMESTAMPTZ)`. Per-batch row.

**New code:**
- `/internal/ingest/`:
  - `validator.go` — per-event validation (`event` + `timestamp` required; `properties` + `extras` lenient).
  - `channel.go` — buffered channel; size from env `INGEST_CHANNEL_SIZE` (default 50000).
  - `flusher.go` — goroutine; flushes on N events (env `INGEST_FLUSH_EVENTS` default 5000) or T seconds (env `INGEST_FLUSH_INTERVAL` default 2s). On CH fail → writes to `failed_events`. On PG-also-fail → sets fatal-state flag.
  - `dlq.go` — drain goroutine; per-row backoff + `attempt_count` increment; quarantine after 20 attempts OR 24h age.
  - `state.go` — `atomic.Bool` fatal-state flag, `atomic.Bool` ingest-disabled flag.
- `/internal/web/middleware.go`: `MaxBody(bytes int64)` factory; CORS middleware.
- `/internal/web/ingest_handlers.go` — public ingest-token auth, check fatal-state + ingest-disabled flags, body limit 10MB.

**Decisions for this step:**
- **Cascade fatal-state flag.** When CH write AND `failed_events` insert both fail in the same flush, set a process-level flag. While set: `/ingest/v1/events` returns 503 + `Retry-After:30`; channel stops accepting new events; `/healthz` also returns 503. Flusher loops with backoff; first successful CH or PG write clears the flag. Honors "never silently drop" — producer keeps retrying, no 202 it doesn't deserve.
- **Poison-pill quarantine.** DLQ rows quarantine after 20 retry attempts OR 24h age. Drain skips quarantined rows. WARN log with row IDs on quarantine; recovery is a manual SQL UPDATE.
- **DLQ depth observability.** Drain loop logs depth (INFO baseline, WARN >100, ERROR >10k). `/healthz` returns 503 when depth exceeds env-configured ceiling (default `DLQ_DEPTH_503_THRESHOLD=100000`).
- **`INGEST_DISABLED` kill switch.** When env set, `/ingest/v1/events` returns 503 + `Retry-After:300`; query/schema and web UI continue. Boot log + periodic WARN every 5 min while disabled; `/healthz` JSON body reports the flag. Operator workflow: `kamal env set INGEST_DISABLED=1 && kamal app restart`.
- **Per-route `MaxBody`.** `/ingest/v1/events` = 10MB, `/api/v1/projects/:project_id/query` = 256KB, web form POSTs = 64KB. 413 on exceed.
- **CORS** on `/ingest/v1/*`, `/api/v1/*`, and `/mcp`: `Access-Control-Allow-Origin: *` default; `Allow-Methods: GET,POST,OPTIONS`; `Allow-Headers: Authorization,Content-Type`. Optional env `ALLOWED_ORIGINS=https://app.example.com,...` to restrict. Web UI routes set no CORS headers (same-origin enforced by browser).
- **SIGTERM choreography.** (1) Close `/ingest/v1/events` (returns 503 for new requests); (2) HTTP server begins shutdown; (3) flusher gets up to env `INGEST_SHUTDOWN_GRACE_SEC` (default 10s) to drain channel to CH; (4) residual events written to `failed_events` as one batch; (5) exit. Coordinates with kamal-proxy `drain_timeout=30s` (set in step 8's `deploy.yml`).
- **Concurrency primitives.** `sync.Once` for flusher startup; `chan struct{}` for shutdown signaling; `atomic.Bool` for fatal-state and ingest-disabled flags. Avoid mutexes on the hot path.
- **Connection pool sizing.** pgx default is `max_conns = max(4, runtime.NumCPU()*4)`; clickhouse-go defaults to 10. Tune both during this step against realistic ingest concurrency.
- **Edge cases:** Empty batch `{"events": []}` → 200 `{accepted: 0}`, not 400. DB error in bearer/project lookup → 503 generic (don't leak DB status).

**Error/rescue:** See Section 2 error registry (E5–E11, E18, E19) in `/Users/jakejohnson/conductor/workspaces/mere-analytics/lansing-v2/TODOS.md` decision history.

**Tests:**
- Unit: validator (required fields, optional fields, extras stored verbatim, per-event errors in a batch).
- Integration: ingest a batch → rows in `events_raw_v1`; bad project token → 401; soft-deleted project → 404; empty batch → 200; oversized body → 413.
- Chaos: stop CH mid-test → batch lands in `failed_events`; restart CH → DLQ drain succeeds; stop both CH + PG → fatal-state flag → `/ingest/v1/events` 503 + `/healthz` 503; restart CH → flag clears; inject a row that always fails CH insert → quarantined after 20 attempts.
- Saturation: fill channel to `N+1` → 503 + `Retry-After`.
- SIGTERM: send 1000 events, SIGTERM immediately → zero data loss (some in CH, rest in `failed_events`).
- `INGEST_DISABLED`: set env → `/ingest/v1/events` 503; query/schema unaffected.

**Done when:** `curl` ingests events; events appear in `events_raw_v1`; CH outage survives via DLQ; both-down state surfaces via 503 + log; SIGTERM doesn't lose data.

**Shipped (PR #12, engineering-review deltas):** The build above is the pre-review plan; the live implementation refined it. The historical record is preserved verbatim; what actually landed:
- **Env renames.** `INGEST_CHANNEL_SIZE` → `INGEST_EVENT_BUFFER` (now an atomic in-flight *event* ceiling, not a channel-slot count; the channel is sized derived from it). `INGEST_SHUTDOWN_GRACE_SEC` → `INGEST_SHUTDOWN_GRACE` (a `time.Duration`, not an int-seconds). New knobs added: `INGEST_MAX_BODY_BYTES` (default 10 MiB) and `INGEST_DLQ_DRAIN_BATCH_LIMIT` (default 10). The Step 8 reference to `INGEST_SHUTDOWN_GRACE_SEC` (drain-timeout note) inherits the same rename.
- **Fatal-state `Retry-After`.** 5s, not 30s (disabled/kill-switch stays 300s; saturation stays 1s).
- **Soft-deleted project → 401, not 404.** The ingest-token lookup excludes soft-deleted projects and collapses to the same uniform 401 as an unknown/revoked token, so project existence isn't leaked via status code.
- **Infra error in token lookup → 500, not "503 generic".** A PG-down lookup is an infrastructure failure surfaced as 500 (distinct from the 503 backpressure used for disabled/fatal/saturation), so an outage doesn't read as a credential-stuffing signal. The 503-only-leaks-no-DB-status intent is preserved — 500 carries a generic body.
- **`last_used_at` tie-in.** This PR also added `oauth_access_tokens.last_used_at`, stamped fire-and-forget by `RequireBearer` via a 60s-throttled UPDATE — satisfying the Step 4 forward-note (above) that the revoked-tokens / connected-apps work needs.

### Step 6 — Query API + schema

**Goal:** `POST /api/v1/projects/:project_id/query` runs arbitrary SQL as `mere_readonly`; tenant isolation is enforced via `additional_table_filters`; `GET /api/v1/projects/:project_id/schema` exposes the queryable catalog; web playground works.

**Implement per:** [Public API](#public-api) + [Multi-tenant isolation](#multi-tenant-isolation).

**Schema additions:** None.

**New code:**
- `/internal/query/executor.go` — accepts `{sql}`; attaches `additional_table_filters` for the queryable analytics table allowlist (`analytics.events_raw_v1` today); attaches per-request CH settings (`max_execution_time=30s`, `max_memory_usage=4GiB`, `max_result_rows=1000000`); runs against `mere_readonly` pool; **streams** `{columns, rows, stats}` response (write JSON envelope incrementally; emit rows directly to `http.ResponseWriter` instead of buffering a `[]map` — a 1M-row buffered response is ~100MB in memory).
- `/internal/query/schema.go` — returns the queryable catalog from the same allowlist used by the executor. It can read ClickHouse metadata with allowlisted `DESCRIBE TABLE` calls through `mere_readonly`, but it must not expose non-allowlisted databases/tables.
- `/internal/web/query_handlers.go` — `POST /api/v1/projects/{project_id}/query` and `GET /api/v1/projects/{project_id}/schema`, bearer-authed with the OAuth middleware; path project must match the OAuth grant.
- Web UI query playground page (textarea + Run button + results table + schema sidebar).

**Decisions for this step:**
- `additional_table_filters` is the isolation mechanism for `/api/v1/projects/:project_id/query` and the MCP query tool. User SQL passes through unmodified; ClickHouse merges the filter at execution.
- **No typed read endpoints.** Do not build events, sessions, persons, or groups read endpoints; the supported read surface is project-scoped query plus schema.
- **Project scoping contract.** Query/schema get `project_id` from the URL, then verify the OAuth access token authorizes that exact project. Unknown, soft-deleted, unauthorized, or token/path mismatch all return `404`.
- **Per-request limits via `WithSettings`** live in the query executor.
- **Response streaming** is required, not optional.
- CH errors returned verbatim — `mere_readonly`'s grants already define the leak surface.
- **Query handler MUST pass `r.Context()` to the CH driver** — verified in a unit test. Client disconnect → CH query KILLed.
- **Single executor, two front doors.** `/api/v1/projects/:project_id/query` and the MCP query tool (step 7) MUST both call `internal/query.Executor` — same function, same pool, same `additional_table_filters` map, same per-request `WithSettings`. No SQL strings, CH driver calls, or settings construction live in `internal/web/query_handlers.go` or `internal/mcp/`; the handlers are thin adapters that translate transport (HTTP body ↔ MCP tool args) into an executor call.
- **Single schema provider, two front doors.** `/api/v1/projects/:project_id/schema` and the MCP schema tool MUST both call `internal/query.Schema` so the table allowlist cannot drift.
- **Tenant-isolation contract test.** `tenant_isolation_test.go` ships in this step. A route-registry pattern enrolls every read surface (`/api/v1/projects/:project_id/query`, `/api/v1/projects/:project_id/schema`, and later every MCP tool that reads analytics data). The test seeds two projects (A, B) with distinct events, calls every enrolled entry point with A's token, and asserts no B-distinguishable string appears. New read routes or tools either enroll or break the test. This is the single most-important test for the project.

**Error/rescue:**
- CH parse error → 400 with CH message verbatim.
- Timeout (`max_execution_time`) → 400 with timeout message.
- OOM (`max_memory_usage`) → 400 with memory message.
- Client disconnect → query cancellation propagates to CH.

**Tests:**
- **Isolation sanity checks**:
  - `SELECT count() FROM analytics.events_raw_v1` returns only one project's rows.
  - Self-join `SELECT * FROM events_raw_v1 a JOIN events_raw_v1 b ON a.distinct_id = b.distinct_id` — both references filtered.
  - Subquery / alias `SELECT * FROM (SELECT * FROM events_raw_v1)` — still filtered.
  - User attempts to override `SETTINGS additional_table_filters = {...}` — our filter still wins.
  - `system.*` tables unreachable to `mere_readonly`.
- **Schema:** `/api/v1/projects/:project_id/schema` lists `events_raw_v1` columns and does not expose non-allowlisted tables.
- **Limits:** 31s query → 400 timeout; 5GiB query → 400 memory.
- **Cancellation:** client disconnect mid-query → CH `KILL QUERY`.
- **Contract test** as described above.

**Done when:** `/api/v1/projects/:project_id/query` runs SQL with tenant isolation; `/api/v1/projects/:project_id/schema` lists the queryable catalog; playground UI works; contract test green.

### Step 7 — MCP

**Goal:** `/mcp` endpoint via `mark3labs/mcp-go`; tools wrap query + schema.

**Implement per:** [Public API](#public-api).

**Schema additions:** None.

**New code:**
- `/internal/mcp/adapter.go` — `RegisterTool(name, handler)` helper wraps every tool handler with `defer/recover`, translates panics to JSON-RPC `internal_error`, logs at ERROR with stack trace. Mounted via the same mux as web routes so `recoverMiddleware` is a second line of defense.
- `/internal/mcp/tools/` — query and schema tool definitions that **call the same `internal/query` executor and schema provider as the HTTP handlers**. Tool handlers are pure adapters: parse the MCP tool args, call the shared service, marshal the result into the MCP response. No SQL, no CH driver calls, no `additional_table_filters` map construction lives in this package — if it does, the isolation contract has two sources of truth and will drift.
- `/internal/web/handlers/mcp.go` — mounts the MCP handler at `/mcp`; bearer auth identical to `/api/v1/*`.

**Decisions for this step:**
- **Single executor/schema provider, two front doors** (see step 6). MCP tools and `/api/v1/*` handlers share `internal/query`. Anything tenant-sensitive (filters, per-request CH limits, pool selection, table allowlists) lives in that package and only that package.
- **Double-recovery** on panics — library bug cannot escape.
- `mark3labs/mcp-go` pinned to a specific **commit SHA** in `go.mod`, not a moving tag.
- CORS on `/mcp` matches `/api/v1/*`.

**Error/rescue:**
- Panic in tool handler → JSON-RPC `internal_error` + ERROR log; not a process crash.
- Malformed JSON-RPC → well-formed JSON-RPC error response.

**Tests:**
- Inject a panic in a tool → JSON-RPC error, process survives, log line present.
- Malformed JSON-RPC payload → proper JSON-RPC error.
- Bearer auth applied to `/mcp`.
- **MCP tools enrolled in the step-6 tenant-isolation contract test.** A/B-seeded run with A's token through every query/schema tool returns no B-distinguishable row.

**Done when:** A Claude MCP client can connect and run query + schema tools against a project.

### Step 8 — Deploy: end-to-end VPS provisioning

**Goal:** A stranger can `kamal setup` on a fresh Hetzner VPS and have a working analytics server in under 10 minutes.

**Implement per:** [Deploy](#deploy).

**Files:**
- `config/deploy.example.yml`, `config/deploy.yml`.
- `config/deploy/postgres/init.sql`.
- `config/deploy/clickhouse/users.xml`, `config/deploy/clickhouse/config.xml`.
- `scripts/operator/reset-password.sql`, `scripts/operator/wipe-project.sql` (or inline in alias).
- `Dockerfile` already exists; refine for production.

**Decisions for this step:**
- **Resolve `mere_readonly` source-of-truth.** Today (steps 1-2 era) the app provisions readonly on every boot (`internal/clickhouse/bootstrap.go:ProvisionReadonlyUser`). When this step ships `users.xml`, choose between:
  - **(A)** `users.xml` defines admin only; app remains the sole source of truth for readonly. **Default recommendation.** Reason: avoids two sources of truth (sneaky failure mode); keeps drift correction; keeps rotation convergence.
  - (B) `users.xml` defines both; app keeps provisioning as belt-and-suspenders. Risk: divergence in botched rotation.
  - (C) `users.xml` defines both; remove app-side provisioning. Cleanest separation but gives up drift correction.
- **Broken-migration recovery docs.** Add a "Recovering from a broken migration" section to `docs/self-host.md` (written in step 10) with explicit `kamal db-console` / `kamal clickhouse-console` recipes for: (1) `migrate force N`, (2) drop / recreate a broken MV, (3) writing the fix-forward migration. Convention is fix-forward, not rollback.
- **Build-time version stamp.** Inject `git describe --tags` via ld-flag (`-X main.Version=...`) in the Dockerfile; `main.go` logs `version=...` on boot; `/healthz` JSON body returns it. Answers "what's deployed right now?" from `kamal logs`.
- **kamal-proxy `drain_timeout=30s`** in `deploy.yml` (must exceed app's `INGEST_SHUTDOWN_GRACE_SEC` from step 5).
- **Operator alias surface:** `reset-password`, `wipe-project`, `db-console`, `clickhouse-console`, plus kamal-default `console` and `logs`. No additional aliases in v1.

**Tests:**
- Manual on a fresh Hetzner VPS: `kamal setup` from zero → TLS → `POST /ingest/v1/events` → `POST /api/v1/projects/:project_id/query` + `GET /api/v1/projects/:project_id/schema` → `kamal deploy` an upgrade with zero downtime → verify ingest + query unaffected during the swap.
- Migration auto-run on container restart: deploy with a no-op new migration; observe it applied on boot.

**Done when:** A fresh VPS reaches working state via `kamal setup` in under 10 min.

### Step 9 — CI image publishing

**Goal:** `ghcr.io/<org>/mere:vX.Y.Z` exists shortly after `git tag v0.1.0 && git push --tags`.

**Files:**
- `.github/workflows/release.yml` — builds amd64 image, pushes to GHCR public on tag push.

**Decisions for this step:**
- **amd64-only in v1.** arm64 deferred until requested. Saves CI matrix + build time.
- **Public images on GHCR**; no auth required for pull.

**Tests:**
- Tag a pre-release (`v0.0.1-pre.1`); verify the image lands at the expected GHCR path; verify `docker pull` works without auth.

**Done when:** A tagged release produces a public image within a few minutes.

### Step 10 — Docs + self-host guide

**Goal:** Someone else can stand it up from scratch without help.

**Files:**
- `docs/api.md` — public API reference (every `/ingest/v1/*` and `/api/v1/*` endpoint, MCP tools, error shapes).
- `docs/self-host.md` — the "from zero" workflow above, expanded; includes the broken-migration recovery recipes (from step 8's decision); includes the backward-compatible-migration convention (see TODOS.md); includes example `pg_dump` + `clickhouse-backup` scripts for non-Hetzner deployers.
- `README.md` — pitch, quickstart, link to `self-host.md`.
- `docs/architecture.md` — written once the design has stabilized through implementation.

**Decisions for this step:**
- Document the **backward-compatible migration convention** prominently: add-column is OK; drop-column requires expand-contract across two deploys; rename = same. Reference from every migration file's header comment.
- **Backups:** Hetzner automated disk-level snapshots by default; example `pg_dump` + `clickhouse-backup` scripts for other providers; no built-in accessory in v1.

**Done when:** A stranger reads the docs and gets a working deploy without asking questions.

## Decisions log (resolved open questions)

| # | Question | Decision |
|---|---|---|
| 1 | Admin role in the app? | **No admin role.** All accounts are regular users scoped to teams + projects. The operator is the person with shell access. |
| 2 | Password reset? | **Skip in-app in v1.** Operator runs `kamal reset-password` (Kamal alias → `pgcrypto` SQL against postgres accessory). |
| 3 | Per-project ClickHouse users? | **No.** Two CH users total: `mere_admin` + `mere_readonly`. Isolation enforced at the app layer. |
| 4 | First-user signup model? | **Operator-bootstrapped, invite-only thereafter.** First user via `kamal create-user` (a SQL alias against the postgres accessory, mirroring `kamal reset-password`). Further users join via team invite links — logged-out invitees create their account inline on `/invites/:token`. The deployer doesn't need to gate the URL to keep strangers out. |
| 5 | Ingest validation strictness? | **Strict on required fields** (`event`, `timestamp`), **lenient on extras** (any extra props accepted). |
| 6 | MCP implementation? | **`github.com/mark3labs/mcp-go`** (community library). |
| 7 | Project deletion? | **Soft delete in Postgres** (`deleted_at`). ClickHouse data persists. Operator wipes manually if they want. |
| 8 | `/v1` versioning? | **Forever-stable.** Breaking changes go to `/v2`. Additive changes within `/v1` allowed. |
| 9 | ClickHouse password into `users.xml`? | **Precompute SHA-256 in `.kamal/secrets`** alongside the plaintext; templated `users.xml` references `${CLICKHOUSE_*_PASSWORD_SHA256}`. Plaintext never hits the CH server. |
| 10 | Migration timing on deploy? | **App auto-runs on container start** (PG then CH). Failed migration aborts startup; kamal-proxy keeps routing to prior version. |
| 11 | Soft-deleted project visibility on private API endpoints? | **404 immediately** on `/api/v1/projects/:project_id/*`. Underlying CH data is retained until `kamal wipe-project` runs. |
| 12 | `failed_events` retention after drain? | **Delete on successful drain.** Pure retry buffer, not an audit trail. |
| 13 | Multi-tenant query isolation? | **ClickHouse `additional_table_filters` setting** attached per request. User SQL is unmodified; CH applies the filter at execution. No SQL parser to maintain. |
| 14 | Team invite flow? | **Invite-by-link.** One-shot token, 7-day TTL, generated from team settings. Inviter shares the URL out-of-band; recipient signs up (or logs in) and joins. |
| 15 | Operator alias surface in v1? | **The five listed:** `create-user`, `reset-password`, `wipe-project`, `db-console`, `clickhouse-console` — plus Kamal-default `console` and `logs`. `create-user` exists because there's no public signup; further user management still happens in the web UI via invite links. |
| 16 | `audit_log` in v1? | **Skip.** Add when an operator asks for it. Not in the Postgres schema. |
| 17 | Image arch in CI? | **amd64 only in v1.** arm64 added when someone asks; saves CI matrix + build time now. |
| 18 | Backup strategy? | **Hetzner automated backups** by default (block-level disk snapshots, enabled in Hetzner Console). Self-hosters on other providers run `pg_dump` + `clickhouse-backup` themselves — example scripts in `docs/self-host.md`. No built-in accessory in v1. |

## Remaining open questions

None — all surfaced questions are now in the Decisions log. New ones will appear during implementation; capture them here as they come up.
