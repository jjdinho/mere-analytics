# Analytics Server — Plan

**Status:** Draft for review
**Last updated:** 2026-05-28

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
│  │   - /v1/ingest     │    │   - signup / login        │   │
│  │   - /v1/events     │    │   - projects + tokens     │   │
│  │   - /v1/sessions   │    │   - event explorer        │   │
│  │   - /v1/persons    │    │   - query playground      │   │
│  │   - /v1/groups     │    │                           │   │
│  │   - /v1/query      │    │                           │   │
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
       │  teams,      │         │   → events_v2    │
       │  projects,   │         │  sessions_v1     │
       │  api_tokens, │         │  persons_v1      │
       │  sessions,   │         │  groups_v1       │
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
  /web/                     # web UI handlers (signup, projects, tokens, explorer, playground)
  /auth/                    # sessions, API tokens, password hashing
  /projects/                # project + token CRUD
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
  /operator/                # SQL scripts invoked by Kamal aliases (reset-password.sql, wipe-project.sql, ...)
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
| `api_tokens` | scoped to a project; bearer tokens for the public API + MCP |
| `sessions` | web UI login sessions |
| `team_invites` | one-shot invite links: `token`, `team_id`, `created_by`, `expires_at`, `consumed_at` |
| `failed_events` | DLQ for ingest batches that failed to land in ClickHouse |

### ClickHouse (analytics)

Versioned tables (suffix `_vN`), raw landing + materialized view pattern for events:

| Table | Type | Notes |
|---|---|---|
| `events_raw_v1` | MergeTree | Landing table. All ingest writes here. |
| `events_v2` | MergeTree via MV | Derived from `events_raw_v1`. The queryable surface. |
| `sessions_v1` | MergeTree via MV | 30-min inactivity model. Keyed on `distinct_id`. |
| `persons_v1` | MergeTree via MV | Thin rollup by `distinct_id`. |
| `groups_v1` | MergeTree | Group properties, written directly. |

All tables include `project_id` as a primary-key prefix component (cheap scoping at the part level).

### ClickHouse users

Exactly two, defined in `config/deploy/clickhouse/users.xml` and provisioned by the ClickHouse container on first start:

- **`mere_admin`** — full DDL/DML. Used for migrations, materialized-view creation, ingest writes.
- **`mere_readonly`** — `SELECT` only. Used by the query executor for all read traffic (`/v1/query`, `/v1/events`, `/v1/sessions`, `/v1/persons`, `/v1/groups`, MCP read tools).

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

All endpoints are project-scoped via API token (Bearer auth). Cursor pagination, ClickHouse error passthrough, JSON in/out.

**Soft-deleted projects return `404 Not Found` on every `/v1/*` endpoint** — the API behaves exactly as if the project never existed. Underlying ClickHouse data is retained until an operator runs `kamal wipe-project`, so a deletion can be undone via direct SQL up to that point.

```
POST  /v1/ingest          # batch events (1..N per request)
GET   /v1/events          # list with filters
GET   /v1/events/:id      # single event
GET   /v1/sessions
GET   /v1/sessions/:id
GET   /v1/persons
GET   /v1/persons/:distinct_id
GET   /v1/groups
POST  /v1/query           # SQL passthrough, runs through CTE-wrapping executor
GET   /mcp                # MCP endpoint — tools wrap the read + query endpoints
```

**Versioning policy:** `/v1` is **forever-stable**. Any breaking change ships at `/v2`. `/v1` and `/v2` coexist indefinitely. Additive changes (new fields, new endpoints) are allowed within `/v1`.

The `/v1/query` body is `{"sql": "..."}`. The response is `{"columns": [...], "rows": [...], "stats": {...}}`. ClickHouse errors are returned with the original message — power users want them.

## Ingest validation

Strict on required fields, lenient on extras:

- **Required**: `event` (string), `timestamp` (ISO 8601 or epoch ms). Project comes from the API token, not the payload.
- **Optional but supported first-class**: `distinct_id`, `properties` (arbitrary JSON), `$session_id`, `$group_*`.
- **Extras**: any other top-level field is stored verbatim in a JSON column. No rejection. Consumers can query their own fields without us shipping a migration.
- **Rejection** = HTTP 400 with a per-event error array; the rest of the batch is accepted. We never silently drop.

## Web UI

Server-rendered (templ) + htmx for interactivity. Pages:

- `/signup`, `/login`, `/logout` — open signup; the deployer fronts the URL with whatever ACL they want (e.g. cloudflare access, basic auth, VPN).
- `/teams/:id` — team settings, member list, "Generate invite link" button. Clicking produces a one-shot URL (`/invites/:token`) that the inviter copies and shares out-of-band. Visiting the URL while logged-out routes to signup; while logged-in, adds the current user to the team. Each token is consumable once and has a 7-day TTL.
- `/teams/:id/projects` — projects in this team; create / soft-delete.
- `/projects/:id` — settings, API tokens (create / revoke).
- `/projects/:id/events` — recent events table, filterable, paginated.
- `/projects/:id/query` — SQL playground (textarea + Run button, results table).

That's the whole UI. CodeMirror or Monaco gets dropped into the query page only if the textarea becomes a real bottleneck.

## Auth

- **Web UI:** session cookie (HttpOnly, SameSite=Lax), backed by `sessions` table.
- **Public API + MCP:** Bearer API tokens, scoped to a project. Tokens are issued in the web UI, stored hashed (sha256) in `api_tokens`.
- **No in-app password reset in v1.** Operators reset via the `kamal reset-password` alias (see "Operator actions"), which executes a SQL `UPDATE` against the `postgres` accessory using `pgcrypto`'s `crypt(..., gen_salt('bf', 10))`. Go's `bcrypt` and `pgcrypto`'s `bf` hashes are wire-compatible (both produce standard `$2a$` format), so the user logs in normally afterwards. The user is forced to change the password on next login.
- **No OAuth in v1.** MCP supports bearer tokens; that's enough.

## Multi-tenant isolation

Application-layer, not database-layer. The mechanism is ClickHouse's **`additional_table_filters`** session setting: the executor attaches a filter to every queryable analytics table for the duration of the query, and ClickHouse transparently applies it to every reference to those tables.

Implementation:

```go
ctx := clickhouse.Context(r.Context(), clickhouse.WithSettings(map[string]any{
    "additional_table_filters": fmt.Sprintf(
        "{'analytics.events_v2': 'project_id = ''%s''', "+
        " 'analytics.sessions_v1': 'project_id = ''%s''', "+
        " 'analytics.persons_v1': 'project_id = ''%s''', "+
        " 'analytics.groups_v1': 'project_id = ''%s'''}",
        projectID, projectID, projectID, projectID,
    ),
}))
rows, err := readonlyPool.QueryContext(ctx, userSQL)
```

The user's SQL is sent through **unmodified** — no parsing, no CTE wrapping, no substring rewriting. ClickHouse merges the filter into the query plan at execution time. The connection runs as `mere_readonly`.

Why this over CTE rewriting:
- No SQL parser to maintain or audit — fewer escape hatches for an attacker to find.
- The filter applies to every reference to the table, including references inside views and joins the user might write.
- Native to ClickHouse; the implementation is a single map literal, not a SQL transformer.

Sanity-check tests for step 8:
- Naive query: `SELECT count() FROM analytics.events_v2` → only this project's rows.
- Cross-table join: `SELECT * FROM events_v2 e JOIN persons_v1 p ON e.distinct_id = p.distinct_id` → both filtered.
- Aliases / `FROM (SELECT ...)` subqueries → still filtered.
- User attempts to bypass via `SETTINGS additional_table_filters = {...}` in their own SQL → ClickHouse merges; our filter still applies (verify).
- Tables not in the map (e.g. `system.numbers`) → not filtered, but `mere_readonly` can't reach them anyway.

**Server-side reads** (the typed `/v1/events`, `/v1/sessions`, etc. endpoints) bypass `additional_table_filters` — they build their own queries from typed inputs and add the `project_id` filter directly. `additional_table_filters` is specifically for the SQL passthrough surface (`/v1/query` and the MCP query tool).

## Ingest reliability

Each `POST /v1/ingest` request:

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
  reset-password: >
    accessory exec postgres -i
    "psql -U $POSTGRES_USER -d $POSTGRES_DB
     -v email=$EMAIL -v password=$NEW_PASSWORD
     -f /operator/reset-password.sql"

  wipe-project: >
    accessory exec clickhouse -i
    "clickhouse-client --query
     \"ALTER TABLE analytics.events_v2 DELETE WHERE project_id = '$PROJECT_ID'\""
```

Operator invocation from their laptop (after `kamal config` is set up):

```bash
EMAIL=user@example.com NEW_PASSWORD=temp-pw-1234 kamal reset-password
PROJECT_ID=01HX... kamal wipe-project
```

Initial v1 alias set:

| Alias | Purpose | Backend |
|---|---|---|
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
```

`kamal setup` runs end-to-end on the VPS:

1. SSHes in as root, installs Docker.
2. Pulls `postgres:16` and starts it as an accessory with `config/deploy/postgres/init.sql` mounted at `/docker-entrypoint-initdb.d/init.sql` (creates the database + `CREATE EXTENSION pgcrypto`).
3. Pulls `clickhouse/clickhouse-server:24.12` and starts it with `config/deploy/clickhouse/users.xml` mounted — provisions `mere_admin` + `mere_readonly` users with SHA-256 hashes of the passwords from `.kamal/secrets`.
4. Builds the Go image from the repo's `Dockerfile`, pushes to the configured registry.
5. Pulls the image on the VPS, starts the app container.
6. App entry point runs pending PG migrations (as the app's PG user) and CH migrations (as `mere_admin`), then starts the HTTP server.
7. `kamal-proxy` fronts the app, terminates TLS via Let's Encrypt for the configured hostname.

After that, subsequent releases are just `kamal deploy`: pulls the new image, restarts the container (which re-runs pending migrations on boot), zero-downtime rollout.

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
  reset-password:    accessory exec postgres -i "psql -U $POSTGRES_USER -d $POSTGRES_DB -v email=$EMAIL -v password=$NEW_PASSWORD -f /operator/reset-password.sql"
  wipe-project:      accessory exec clickhouse -i "clickhouse-client --query \"ALTER TABLE analytics.events_v2 DELETE WHERE project_id = '$PROJECT_ID'\""
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

### Step 3 — Auth

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
- CSRF: per-session token in `sessions.csrf_token`; templ `@csrfField()` helper for forms; htmx layout sets `hx-headers='{"X-CSRF-Token":"..."}'` globally; middleware verifies on non-GET requests to web routes; `/v1/*` and `/mcp` exempt (Bearer auth, no cookie).
- Open signup; bot-flood mitigation is the deployer's responsibility (front with Cloudflare Access / basic auth / VPN). Plan Non-goals + decisions log #4.
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

### Step 4 — Teams + projects + tokens

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
- Bearer auth lookup against soft-deleted project: 404 on `/v1/*` (plan line 209); token itself isn't revoked, just inaccessible.

**Error/rescue:**
- Token "Create" double-click → `hx-disable-elt='this'` on every htmx button (templ helper).
- Direct object reference for someone else's project → viewer returns no rows → 404.
- Invite already-consumed / expired → 404 (don't distinguish).

**Tests:**
- Unit: token format/length, sha256 round-trip, invite expiry math.
- Integration: token-create flow returns plaintext exactly once; subsequent loads never expose plaintext; project soft-delete makes `/v1/*` return 404; invite consume-once race (two concurrent `UPDATE` → only one wins).
- Security: cross-user authorization on every `/teams/:id` and `/projects/:id` route — user A cannot view user B's data.

**Done when:** Create a team via UI, invite a second user, create a project, issue a token (visible once), revoke it, soft-delete the project.

### Step 5 — Ingest

**Goal:** `curl POST /v1/ingest` lands events in `events_raw_v1`. Survives transient CH outages via DLQ; surfaces both-down state loudly.

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
- `/internal/web/handlers/ingest.go` — bearer auth, check fatal-state + ingest-disabled flags, body limit 10MB.

**Decisions for this step:**
- **Cascade fatal-state flag.** When CH write AND `failed_events` insert both fail in the same flush, set a process-level flag. While set: `/v1/ingest` returns 503 + `Retry-After:30`; channel stops accepting new events; `/healthz` also returns 503. Flusher loops with backoff; first successful CH or PG write clears the flag. Honors "never silently drop" — producer keeps retrying, no 202 it doesn't deserve.
- **Poison-pill quarantine.** DLQ rows quarantine after 20 retry attempts OR 24h age. Drain skips quarantined rows. WARN log with row IDs on quarantine; recovery is a manual SQL UPDATE.
- **DLQ depth observability.** Drain loop logs depth (INFO baseline, WARN >100, ERROR >10k). `/healthz` returns 503 when depth exceeds env-configured ceiling (default `DLQ_DEPTH_503_THRESHOLD=100000`).
- **`INGEST_DISABLED` kill switch.** When env set, `/v1/ingest` returns 503 + `Retry-After:300`; `/v1/query` and web UI continue. Boot log + periodic WARN every 5 min while disabled; `/healthz` JSON body reports the flag. Operator workflow: `kamal env set INGEST_DISABLED=1 && kamal app restart`.
- **Per-route `MaxBody`.** `/v1/ingest` = 10MB, `/v1/query` = 256KB, web form POSTs = 64KB. 413 on exceed.
- **CORS** on `/v1/*` and `/mcp`: `Access-Control-Allow-Origin: *` default; `Allow-Methods: GET,POST,OPTIONS`; `Allow-Headers: Authorization,Content-Type`. Optional env `ALLOWED_ORIGINS=https://app.example.com,...` to restrict. Web UI routes set no CORS headers (same-origin enforced by browser).
- **SIGTERM choreography.** (1) Close `/v1/ingest` (returns 503 for new requests); (2) HTTP server begins shutdown; (3) flusher gets up to env `INGEST_SHUTDOWN_GRACE_SEC` (default 10s) to drain channel to CH; (4) residual events written to `failed_events` as one batch; (5) exit. Coordinates with kamal-proxy `drain_timeout=30s` (set in step 10's `deploy.yml`).
- **Concurrency primitives.** `sync.Once` for flusher startup; `chan struct{}` for shutdown signaling; `atomic.Bool` for fatal-state and ingest-disabled flags. Avoid mutexes on the hot path.
- **Connection pool sizing.** pgx default is `max_conns = max(4, runtime.NumCPU()*4)`; clickhouse-go defaults to 10. Tune both during this step against realistic ingest concurrency.
- **Edge cases:** Empty batch `{"events": []}` → 200 `{accepted: 0}`, not 400. DB error in bearer/project lookup → 503 generic (don't leak DB status).

**Error/rescue:** See Section 2 error registry (E5–E11, E18, E19) in `/Users/jakejohnson/conductor/workspaces/mere-analytics/lansing-v2/TODOS.md` decision history.

**Tests:**
- Unit: validator (required fields, optional fields, extras stored verbatim, per-event errors in a batch).
- Integration: ingest a batch → rows in `events_raw_v1`; bad project token → 401; soft-deleted project → 404; empty batch → 200; oversized body → 413.
- Chaos: stop CH mid-test → batch lands in `failed_events`; restart CH → DLQ drain succeeds; stop both CH + PG → fatal-state flag → `/v1/ingest` 503 + `/healthz` 503; restart CH → flag clears; inject a row that always fails CH insert → quarantined after 20 attempts.
- Saturation: fill channel to `N+1` → 503 + `Retry-After`.
- SIGTERM: send 1000 events, SIGTERM immediately → zero data loss (some in CH, rest in `failed_events`).
- `INGEST_DISABLED`: set env → `/v1/ingest` 503; `/v1/query` unaffected.

**Done when:** `curl` ingests events; events appear in `events_raw_v1`; CH outage survives via DLQ; both-down state surfaces via 503 + log; SIGTERM doesn't lose data.

### Step 6 — Events MV + read endpoints

**Goal:** `events_v2` MV is populated from `events_raw_v1`; `GET /v1/events` and the web event explorer return data scoped to the bearer-authed project.

**Implement per:** [Data model](#data-model) + [Multi-tenant isolation](#multi-tenant-isolation).

**Schema additions:**
- New CH migration: `events_v2` table + materialized view from `events_raw_v1`.
  - Columns: `(project_id UUID, event LowCardinality(String), distinct_id Nullable(String), timestamp DateTime64(3,'UTC'), session_id Nullable(String), properties String, extras String)` — same as `events_raw_v1` minus `received_at`.
  - `properties` and `extras` stay as raw JSON `String`: lossless; user calls `JSONExtract*` in `/v1/query`.
  - `ORDER BY (project_id, event, timestamp)` — read-optimized for "find all events of type X". Revisit with realistic data.
  - `PARTITION BY toYYYYMM(timestamp)`.

**New code:**
- `/internal/events/scoped.go` — `events.Scoped(ctx, projectID)` builder exposing `.List(filter)`, `.ByID(id)`, `.Count(...)`, etc. Every method generates SQL with `WHERE project_id = $projectID` built in. Constructor attaches `clickhouse.WithSettings({max_execution_time=30, max_memory_usage=4GiB, max_result_rows=1000000})` so tenant filter and query budget ride together — no callsite can have one without the other.
- `/internal/web/handlers/events.go` — `GET /v1/events`, `GET /v1/events/:id` (bearer-authed, calls `Scoped`); web UI event explorer page.

**Decisions for this step:**
- **`events_v2` schema as above** — raw `String` for `properties`/`extras` (lossless; conflicts with naïve "queryable" framing but user has `JSONExtract*` in `/v1/query`). MV's only job is read-optimized ORDER BY.
- **`Scoped` builder pattern** is the compile-time-ish guarantee against cross-tenant leak. No method in the package exposes a query without the `project_id` predicate. Mirror this pattern in step 7 for sessions/persons/groups.
- **Per-request CH limits via `WithSettings`** attached by `Scoped` constructor. Implementer cannot forget — there is no other way to obtain a query.
- **Pagination:** cursor = `base64(timestamp + id)`; bad cursor → 400 `invalid_cursor`.
- **Verify golang-migrate CH multi-statement.** The driver is already configured with `MultiStatementEnabled: true` (`internal/clickhouse/clickhouse.go:63`), but smoke-test with `CREATE TABLE` + `CREATE MATERIALIZED VIEW` in one migration file before relying on it. If it fails, split into two files.

**Error/rescue:**
- Bad cursor → 400.
- Forgotten WHERE — structurally prevented by `Scoped`.
- Query timeout / OOM — surfaced as 400 with CH error verbatim.

**Tests:**
- MV idempotency: boot twice, no errors, MV state identical.
- `Scoped` enforcement: project A's `Scoped().List()` never returns project B's rows; constructor with empty `projectID` → runtime error.
- Pagination round-trip.
- Integration: ingest 1000 events into project A, 1000 into B; list with A's token returns exactly 1000.

**Done when:** Ingest events; see them via `/v1/events` and in the explorer UI.

### Step 7 — Sessions + persons + groups

**Goal:** `sessions_v1`, `persons_v1`, `groups_v1` exist and back read endpoints.

**Implement per:** [Data model](#data-model).

**Schema additions:**
- New CH migrations: `sessions_v1` (MV from `events_v2`, 30-min inactivity model), `persons_v1` (MV from `events_v2` keyed on `distinct_id`), `groups_v1` (MergeTree; direct writes from a forthcoming `POST /v1/groups` endpoint).
- All carry `project_id` as PK prefix.

**New code:**
- `/internal/sessions/scoped.go`, `/internal/persons/scoped.go`, `/internal/groups/scoped.go` — `Scoped` builder per resource, mirroring step 6.
- `/internal/web/handlers/` — `GET /v1/sessions`, `GET /v1/sessions/:id`, `GET /v1/persons`, `GET /v1/persons/:distinct_id`, `GET /v1/groups`.

**Decisions for this step:**
- Same `Scoped` pattern + same per-request CH limits as step 6.
- Groups: `POST /v1/groups` for direct upserts (no MV); request shape decided during implementation.

**Tests:**
- MV idempotency per table.
- `Scoped` enforcement per resource.
- Session boundary: 30-min inactivity correctly splits one user's events into two sessions.

**Done when:** Ingest events; see derived sessions/persons/groups data via API.

### Step 8 — Query API

**Goal:** `POST /v1/query` runs arbitrary SQL as `mere_readonly`; tenant isolation enforced via `additional_table_filters`; web playground works.

**Implement per:** [Multi-tenant isolation](#multi-tenant-isolation).

**Schema additions:** None.

**New code:**
- `/internal/query/executor.go` — accepts `{sql}`; attaches `additional_table_filters` for all analytics tables; attaches per-request CH settings (`max_execution_time=30s`, `max_memory_usage=4GiB`, `max_result_rows=1000000`); runs against `mere_readonly` pool; **streams** `{columns, rows, stats}` response (write JSON envelope incrementally; emit rows directly to `http.ResponseWriter` instead of buffering a `[]map` — a 1M-row buffered response is ~100MB in memory).
- `/internal/web/handlers/query.go` — `POST /v1/query`; web UI playground page (textarea + Run button + results table).

**Decisions for this step:**
- `additional_table_filters` is the isolation mechanism for `/v1/query` and the MCP query tool only — typed reads (steps 6-7) use `Scoped` builders instead. User SQL passes through unmodified; CH merges the filter at execution.
- **Per-request limits via `WithSettings`** (same values as `Scoped`).
- **Response streaming** is required, not optional.
- CH errors returned verbatim — `mere_readonly`'s grants already define the leak surface.
- **Query handler MUST pass `r.Context()` to the CH driver** — verified in a unit test. Client disconnect → CH query KILLed.
- **Single executor, two front doors.** `/v1/query` and the MCP query tool (step 9) MUST both call `internal/query.Executor` — same function, same pool, same `additional_table_filters` map, same per-request `WithSettings`. No SQL strings, CH driver calls, or settings construction live in `internal/web/handlers/query.go` or `internal/mcp/`; the handlers are thin adapters that translate transport (HTTP body ↔ MCP tool args) into an executor call. Same rule for typed reads: `internal/{events,sessions,persons,groups}` `Scoped` builders are the only path to the analytics tables. If a future "MCP shortcut" is tempting, the answer is to extend the executor, not duplicate it.
- **Tenant-isolation contract test.** `tenant_isolation_test.go` ships in this step. A route-registry pattern enrolls every `/v1/*` route **and every MCP tool that reads analytics data** (query, events, sessions, persons, groups). The test seeds two projects (A, B) with distinct events, calls every enrolled entry point with A's token, and asserts no B-distinguishable string appears. New routes or tools either enroll or break the test. This is the single most-important test for the project.

**Error/rescue:**
- CH parse error → 400 with CH message verbatim.
- Timeout (`max_execution_time`) → 400 with timeout message.
- OOM (`max_memory_usage`) → 400 with memory message.
- Client disconnect → query cancellation propagates to CH.

**Tests:**
- **Isolation sanity checks** (plan lines 282–288):
  - `SELECT count() FROM analytics.events_v2` returns only one project's rows.
  - Cross-table join `SELECT * FROM events_v2 e JOIN persons_v1 p ON e.distinct_id = p.distinct_id` — both filtered.
  - Subquery / alias `SELECT * FROM (SELECT * FROM events_v2)` — still filtered.
  - User attempts to override `SETTINGS additional_table_filters = {...}` — our filter still wins.
  - `system.*` tables unreachable to `mere_readonly`.
- **Limits:** 31s query → 400 timeout; 5GiB query → 400 memory.
- **Cancellation:** client disconnect mid-query → CH `KILL QUERY`.
- **Contract test** as described above.

**Done when:** `/v1/query` runs SQL with tenant isolation; playground UI works; contract test green.

### Step 9 — MCP

**Goal:** `/mcp` endpoint via `mark3labs/mcp-go`; tools wrap the read + query endpoints.

**Implement per:** [Public API](#public-api).

**Schema additions:** None.

**New code:**
- `/internal/mcp/adapter.go` — `RegisterTool(name, handler)` helper wraps every tool handler with `defer/recover`, translates panics to JSON-RPC `internal_error`, logs at ERROR with stack trace. Mounted via the same mux as web routes so `recoverMiddleware` is a second line of defense.
- `/internal/mcp/tools/` — tool definitions that **call the same `internal/query` executor and `internal/{events,sessions,persons,groups}` `Scoped` builders as the HTTP handlers**. Tool handlers are pure adapters: parse the MCP tool args, call the shared service, marshal the result into the MCP response. No SQL, no CH driver calls, no `additional_table_filters` map construction lives in this package — if it does, the isolation contract has two sources of truth and will drift.
- `/internal/web/handlers/mcp.go` — mounts the MCP handler at `/mcp`; bearer auth identical to `/v1/*`.

**Decisions for this step:**
- **Single executor, two front doors** (see step 8). MCP tools and `/v1/*` handlers share `internal/query` and the typed `Scoped` builders. Anything tenant-sensitive (filters, per-request CH limits, pool selection) lives in those packages and only those packages.
- **Double-recovery** on panics — library bug cannot escape.
- `mark3labs/mcp-go` pinned to a specific **commit SHA** in `go.mod`, not a moving tag.
- CORS on `/mcp` matches `/v1/*`.

**Error/rescue:**
- Panic in tool handler → JSON-RPC `internal_error` + ERROR log; not a process crash.
- Malformed JSON-RPC → well-formed JSON-RPC error response.

**Tests:**
- Inject a panic in a tool → JSON-RPC error, process survives, log line present.
- Malformed JSON-RPC payload → proper JSON-RPC error.
- Bearer auth applied to `/mcp`.
- **MCP tools enrolled in the step-8 tenant-isolation contract test.** A/B-seeded run with A's token through every read/query tool returns no B-distinguishable row.

**Done when:** A Claude MCP client can connect and run a read tool against a project.

### Step 10 — Deploy: end-to-end VPS provisioning

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
- **Broken-migration recovery docs.** Add a "Recovering from a broken migration" section to `docs/self-host.md` (written in step 12) with explicit `kamal db-console` / `kamal clickhouse-console` recipes for: (1) `migrate force N`, (2) drop / recreate a broken MV, (3) writing the fix-forward migration. Convention is fix-forward, not rollback.
- **Build-time version stamp.** Inject `git describe --tags` via ld-flag (`-X main.Version=...`) in the Dockerfile; `main.go` logs `version=...` on boot; `/healthz` JSON body returns it. Answers "what's deployed right now?" from `kamal logs`.
- **kamal-proxy `drain_timeout=30s`** in `deploy.yml` (must exceed app's `INGEST_SHUTDOWN_GRACE_SEC` from step 5).
- **Operator alias surface:** `reset-password`, `wipe-project`, `db-console`, `clickhouse-console`, plus kamal-default `console` and `logs`. No additional aliases in v1.

**Tests:**
- Manual on a fresh Hetzner VPS: `kamal setup` from zero → TLS → `POST /v1/ingest` → `GET /v1/events` → `kamal deploy` an upgrade with zero downtime → verify ingest + query unaffected during the swap.
- Migration auto-run on container restart: deploy with a no-op new migration; observe it applied on boot.

**Done when:** A fresh VPS reaches working state via `kamal setup` in under 10 min.

### Step 11 — CI image publishing

**Goal:** `ghcr.io/<org>/mere:vX.Y.Z` exists shortly after `git tag v0.1.0 && git push --tags`.

**Files:**
- `.github/workflows/release.yml` — builds amd64 image, pushes to GHCR public on tag push.

**Decisions for this step:**
- **amd64-only in v1.** arm64 deferred until requested. Saves CI matrix + build time.
- **Public images on GHCR**; no auth required for pull.

**Tests:**
- Tag a pre-release (`v0.0.1-pre.1`); verify the image lands at the expected GHCR path; verify `docker pull` works without auth.

**Done when:** A tagged release produces a public image within a few minutes.

### Step 12 — Docs + self-host guide

**Goal:** Someone else can stand it up from scratch without help.

**Files:**
- `docs/api.md` — public API reference (every `/v1/*` endpoint, MCP tools, error shapes).
- `docs/self-host.md` — the "from zero" workflow above, expanded; includes the broken-migration recovery recipes (from step 10's decision); includes the backward-compatible-migration convention (see TODOS.md); includes example `pg_dump` + `clickhouse-backup` scripts for non-Hetzner deployers.
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
| 4 | First-user signup model? | **Open signup.** Deployer fronts the URL with their own ACL if they want to gate it. |
| 5 | Ingest validation strictness? | **Strict on required fields** (`event`, `timestamp`), **lenient on extras** (any extra props accepted). |
| 6 | MCP implementation? | **`github.com/mark3labs/mcp-go`** (community library). |
| 7 | Project deletion? | **Soft delete in Postgres** (`deleted_at`). ClickHouse data persists. Operator wipes manually if they want. |
| 8 | `/v1` versioning? | **Forever-stable.** Breaking changes go to `/v2`. Additive changes within `/v1` allowed. |
| 9 | ClickHouse password into `users.xml`? | **Precompute SHA-256 in `.kamal/secrets`** alongside the plaintext; templated `users.xml` references `${CLICKHOUSE_*_PASSWORD_SHA256}`. Plaintext never hits the CH server. |
| 10 | Migration timing on deploy? | **App auto-runs on container start** (PG then CH). Failed migration aborts startup; kamal-proxy keeps routing to prior version. |
| 11 | Soft-deleted project visibility on `/v1/*`? | **404 immediately** on all endpoints. Underlying CH data is retained until `kamal wipe-project` runs. |
| 12 | `failed_events` retention after drain? | **Delete on successful drain.** Pure retry buffer, not an audit trail. |
| 13 | Multi-tenant query isolation? | **ClickHouse `additional_table_filters` setting** attached per request. User SQL is unmodified; CH applies the filter at execution. No SQL parser to maintain. |
| 14 | Team invite flow? | **Invite-by-link.** One-shot token, 7-day TTL, generated from team settings. Inviter shares the URL out-of-band; recipient signs up (or logs in) and joins. |
| 15 | Operator alias surface in v1? | **The four listed:** `reset-password`, `wipe-project`, `db-console`, `clickhouse-console` — plus Kamal-default `console` and `logs`. No user-management aliases until asked for. |
| 16 | `audit_log` in v1? | **Skip.** Add when an operator asks for it. Not in the Postgres schema. |
| 17 | Image arch in CI? | **amd64 only in v1.** arm64 added when someone asks; saves CI matrix + build time now. |
| 18 | Backup strategy? | **Hetzner automated backups** by default (block-level disk snapshots, enabled in Hetzner Console). Self-hosters on other providers run `pg_dump` + `clickhouse-backup` themselves — example scripts in `docs/self-host.md`. No built-in accessory in v1. |

## Remaining open questions

None — all surfaced questions are now in the Decisions log. New ones will appear during implementation; capture them here as they come up.
