# Self-hosting mere

mere deploys with [Kamal](https://kamal-deploy.org). One `kamal setup` brings up
the app, Postgres, and ClickHouse on a fresh VPS, fronted by `kamal-proxy` with
automatic Let's Encrypt TLS. You build and push the image **from your own
machine** with `kamal deploy`; there is no CI pipeline and no pre-built public
image — the first deploy builds the image once (a couple of minutes), and every
subsequent `kamal deploy` rebuilds and rolls it out with zero downtime.

This matches how the binary is meant to be operated: the "operator" is simply
whoever has `kamal` config plus SSH to the box. There is no in-app admin role.

## Prerequisites

- A Linux VPS with root SSH (Hetzner is the reference target, but any will do).
- A DNS hostname pointed at the VPS (for TLS).
- Docker + Ruby/Kamal on your **local** machine (`gem install kamal`).
- A container registry you can push to (GHCR by default).

## From zero

```bash
# 1. Clone.
git clone https://github.com/jjdinho/mere-analytics && cd mere-analytics

# 2. Create your deploy config from the template and fill in the placeholders.
cp config/deploy.example.yml config/deploy.yml
#   edit config/deploy.yml — replace every <PLACEHOLDER>:
#     image:                  <GHCR_USERNAME>/mere
#     servers.web.hosts:      <VPS_IP>
#     registry.username:      <GHCR_USERNAME>
#     proxy.host:             <HOSTNAME>
#     accessories.*.host:     <VPS_IP>   (postgres + clickhouse)
#     env.clear.OAUTH_ISSUER_URL: https://<HOSTNAME>
#   (config/deploy.yml is gitignored — it's your machine's copy.)
```

### 3. Secrets

Kamal reads `.kamal/secrets` at deploy time. The ClickHouse `users.xml` expects
the admin password as a **SHA-256 hex digest** (the plaintext never reaches the
CH server), so precompute it alongside the plaintext. `mere_readonly` is
provisioned by the app from its plaintext password, so it needs no digest.

```bash
mkdir -p .kamal && cat > .kamal/secrets <<'EOF'
KAMAL_REGISTRY_PASSWORD=$(op read "op://Personal/GHCR/token")
POSTGRES_PASSWORD=$(openssl rand -hex 32)
CLICKHOUSE_ADMIN_PASSWORD=$(openssl rand -hex 32)
CLICKHOUSE_ADMIN_PASSWORD_SHA256=$(printf %s "$CLICKHOUSE_ADMIN_PASSWORD" | sha256sum | cut -d' ' -f1)
CLICKHOUSE_READONLY_PASSWORD=$(openssl rand -hex 32)
EOF
```

The single-quoted heredoc writes the `$(…)` expressions **literally**; Kamal
evaluates them (and the cross-references like `$CLICKHOUSE_ADMIN_PASSWORD`) when
it reads the file. Swap the `op read` line for however you fetch your registry
token. `.kamal/` is gitignored.

### 4. Bring it up

```bash
kamal setup
```

This SSHes in, installs Docker, starts the Postgres and ClickHouse accessories
(mounting `init.sql` / `users.xml` / `config.xml` and the operator scripts),
builds and pushes the app image, boots the container — which **auto-runs
migrations on start** — and fronts it with `kamal-proxy` + TLS.

### 5. Create the first user

There is no public signup. Bootstrap the first account, then everyone else joins
via invite links from the web UI.

```bash
EMAIL=admin@example.com INITIAL_PASSWORD=change-me-please-1234 kamal create-user
```

The new user is flagged `must_change_password`, so they're forced to rotate it
on first login. Passwords must be at least 12 characters.

You now have a working deployment. Log in at `https://<HOSTNAME>/login`, create a
project, and copy its public ingest token from the project page.

## Upgrades

```bash
git pull
kamal deploy
```

Kamal rebuilds the image (stamping `VERSION` from `git describe`), pushes it,
and does a zero-downtime rollout. The new container re-runs any pending
migrations on boot. Confirm what's live via `GET /healthz` (`"version"` field)
or `kamal logs` (`starting mere-server version=…`).

## Environment reference

Set in `config/deploy.yml` under `env.clear` (non-secret) or `env.secret`
(from `.kamal/secrets`). Required vars have no default.

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | HTTP listen port. |
| `SECURE_COOKIES` | `true` | `Secure` flag on session/CSRF cookies. Keep `true` behind TLS. |
| `POSTGRES_HOST` | *(required)* | |
| `POSTGRES_PORT` | `5432` | |
| `POSTGRES_DB` | `mere` | |
| `POSTGRES_USER` | `mere` | |
| `POSTGRES_PASSWORD` | *(required)* | |
| `CLICKHOUSE_HOST` | *(required)* | |
| `CLICKHOUSE_PORT` | `9000` | Native protocol port. |
| `CLICKHOUSE_DATABASE` | `analytics` | |
| `CLICKHOUSE_ADMIN_USER` | `mere_admin` | Migrations + ingest writes. |
| `CLICKHOUSE_ADMIN_PASSWORD` | *(required)* | |
| `CLICKHOUSE_READONLY_USER` | `mere_readonly` | Query reads (app-provisioned). |
| `CLICKHOUSE_READONLY_PASSWORD` | *(required)* | |
| `OAUTH_ISSUER_URL` | *(required)* | Externally reachable base URL; must match `proxy.host`. Advertised in OAuth discovery and signed into redirects. |
| `OAUTH_ACCESS_TOKEN_TTL` | `1h` | Bearer token lifetime. |
| `OAUTH_AUTHORIZATION_CODE_TTL` | `10m` | Auth-code lifetime. |
| `INGEST_EVENT_BUFFER` | `50000` | In-flight event ceiling; over it, ingest returns `503`. |
| `INGEST_FLUSH_EVENTS` | `5000` | Flush when the buffer reaches this many events… |
| `INGEST_FLUSH_INTERVAL` | `2s` | …or this long elapses, whichever first. |
| `INGEST_SHUTDOWN_GRACE` | `10s` | SIGTERM drain budget before residual events go to the DLQ. |
| `INGEST_DISABLED` | `false` | Kill switch: `/api/v1/ingest/events` returns `503` while set; query/UI keep working. |
| `INGEST_MAX_BODY_BYTES` | `10485760` | Max ingest request body (10 MiB). |
| `INGEST_DLQ_DRAIN_BATCH_LIMIT` | `10` | DLQ rows drained per pass. |
| `DLQ_DEPTH_503_THRESHOLD` | `100000` | `failed_events` depth above which `/healthz` returns `503`. |
| `ALLOWED_ORIGINS` | *(empty → `*`)* | Comma-separated exact origins for CORS on ingest/API. |
| `QUERY_MAX_BODY_BYTES` | `262144` | Max query request body (256 KiB). |
| `QUERY_MAX_RESULT_ROWS` | `1000` | Max rows ClickHouse may return for API, MCP, and playground queries. |
| `QUERY_MAX_EXECUTION_TIME` | `1m` | Max ClickHouse query runtime for API, MCP, and playground queries. |

### Tuning ingest throughput

Ingest is an in-process buffered pipeline: accepted events sit in an in-flight
buffer and are flushed to ClickHouse in batches. Three knobs govern it, and the
right values depend on **your hardware and how fast your ClickHouse can absorb
inserts** — there is no single correct setting.

| Variable | What it controls |
|---|---|
| `INGEST_EVENT_BUFFER` | Ceiling of events held in memory awaiting a flush. When full, ingest sheds load with `503 ingest channel full` + `Retry-After: 1` (well-behaved clients retry — see the [API retry contract](api.md#retrying-ingest--clients-must-handle-503)). |
| `INGEST_FLUSH_EVENTS` | Flush as soon as this many events accumulate. |
| `INGEST_FLUSH_INTERVAL` | Flush at least this often, even when below `INGEST_FLUSH_EVENTS`. |

How they relate:

- **Sustained throughput is bounded by ClickHouse, not by these knobs.** The
  pipeline can only accept events as fast as ClickHouse drains them. A bigger
  buffer absorbs longer *bursts*; it does not raise the steady-state rate. If
  inserts are slow, find out why (disk I/O, an undersized CH node, contention)
  before raising the buffer — a large buffer in front of a slow CH just delays
  the `503`s and grows memory use.
- **`INGEST_EVENT_BUFFER` is your burst headroom.** Size it to roughly the
  largest spike you want to absorb without `503`s, given your drain rate — e.g.
  if CH comfortably ingests ~100k events/s and you want to ride out a 2-second
  spike, ~200k of buffer covers it. Each buffered event costs memory, so don't
  set it arbitrarily high; size it to the spike and the RAM you have.
- **`INGEST_FLUSH_EVENTS` trades batch size against latency.** Larger flushes
  are more efficient for ClickHouse (fewer, bigger inserts) but hold events in
  memory longer; smaller flushes write sooner. Keep it well below
  `INGEST_EVENT_BUFFER` so the buffer can hold several pending flushes.
- **`INGEST_FLUSH_INTERVAL`** bounds how long a low-traffic event waits before
  it's written, independent of volume.

Practical method: load-test against *your* ClickHouse with a retrying client,
watch the sustained `events/s` and the `503` rate, and raise
`INGEST_EVENT_BUFFER` / `INGEST_FLUSH_EVENTS` until bursts stop producing `503`s.
If `503`s persist at steady state (not just on spikes), ClickHouse is the
bottleneck — scale it rather than the buffer. Regardless of tuning, clients must
still retry on `503`; the buffer reduces how often they have to.

## Operator actions

Anything you can't do through the web UI is a **Kamal alias** that `exec`s into
an accessory and runs a parameterised SQL script. The aliases are defined in
`config/deploy.yml`.

```bash
# Create a user (with a personal team + membership). Forced password change on
# first login. Password ≥ 12 chars. Exits non-zero on duplicate email.
EMAIL=user@example.com INITIAL_PASSWORD=change-me-please-1234 kamal create-user

# Force-reset a user's password. Exits non-zero on unknown email.
EMAIL=user@example.com NEW_PASSWORD=change-me-please-1234 kamal reset-password

# Permanently delete a soft-deleted project's events from ClickHouse.
PROJECT_ID=018f… kamal wipe-project

# Interactive consoles.
kamal db-console            # psql against the postgres accessory
kamal clickhouse-console    # clickhouse-client against the clickhouse accessory
kamal console               # sh inside the app container
kamal logs                  # follow app logs
```

`wipe-project` is the irreversible counterpart to the UI's soft-delete: soft
delete hides a project from the API immediately, but the ClickHouse rows live on
until you run this. (The deletion is an async ClickHouse mutation — rows
disappear once it completes.)

### Periodic maintenance (optional but recommended)

The image ships a second binary, `/mere-maintenance`, that sweeps expired
`oauth_codes`, `oauth_access_tokens`, and `sessions` rows out of Postgres. It's
a one-shot (not a daemon) and reuses the app's environment. Run it on a
schedule, e.g. daily, from the operator's machine or a host cron:

```bash
kamal app exec --reuse "/mere-maintenance"
```

Skipping it is harmless — expired rows are never honored at auth time — but they
accumulate, so a periodic sweep keeps the tables tidy.

## Local development

```bash
./scripts/dev
```

Brings up Postgres (host port `55432`) and ClickHouse (`58123` HTTP / `59000`
native) from `docker/docker-compose.yml` and runs the server on `:8080` with
`SECURE_COOKIES=false` for plaintext localhost. Seed a dev user:

```bash
psql "postgresql://mere:devpass@127.0.0.1:55432/mere" \
  -v email=you@example.com -v password=change-me-please \
  -f scripts/operator/create-user.sql
```

## Migrations

Migrations live in `migrations/postgres/` and `migrations/clickhouse/`
(plain `golang-migrate` SQL files), embedded in the binary. On **every**
container start the binary runs pending Postgres migrations (as the app user),
then pending ClickHouse migrations (as `mere_admin`), then starts serving.

A failed migration **aborts startup**: the container exits, `kamal-proxy` keeps
routing to the previous version, and the error shows in `kamal logs`. The
convention is **fix-forward, never rollback**.

### Backward-compatible migrations

Because the old container can still be serving while the new one boots and
migrates, migrations must be **backward-compatible / append-only**:

- **Adding** a column or table is safe.
- **Dropping** a column/table requires expand-contract across **two** deploys:
  first stop reading/writing it, ship; then drop it, ship.
- **Renaming** is the same as a drop — do it as add-new + backfill + drop-old.

### Recovering from a dirty migration

If a migration fails partway, `golang-migrate` marks the version **dirty** and
the next boot refuses to proceed with a message like:

```
ch migrate: version 2 is DIRTY (a prior run failed mid-apply). Inspect the
schema then force the version with:
  migrate -path migrations/clickhouse -database <dsn> force 2
(or use the corresponding kamal db/clickhouse-console alias).
```

To recover:

1. Open the affected database (`kamal db-console` or `kamal clickhouse-console`).
2. Inspect the schema and finish or undo whatever the failed migration left
   half-applied, by hand, until the schema matches the intended end state of
   that version.
3. Clear the dirty flag so the binary will boot — either run `migrate … force N`
   from a machine with the `golang-migrate` CLI pointed at the database, or
   update the migration bookkeeping table directly (Postgres:
   `UPDATE schema_migrations SET dirty = false;`). Then write a **fix-forward**
   migration if the schema needs further changes.

## Backups

**Recommended: Hetzner automated backups.** Enable them on the VPS in the
Hetzner Cloud Console (Server → Backups). Because `/data/mere/postgres` and
`/data/mere/clickhouse` sit on the same disk, the daily snapshot covers both
databases consistently. No in-app backup accessory ships in v1.

**Logical backups** (other providers, or if you want portable dumps):

```bash
# Postgres — custom-format dump, streamed back to your machine.
kamal accessory exec postgres -i \
  "pg_dump -U mere -d mere --format=custom" > mere-pg-$(date +%F).dump

# ClickHouse — stream the events table out in the native format.
kamal accessory exec clickhouse -i \
  "clickhouse-client --query 'SELECT * FROM analytics.events_raw_v1 FORMAT Native'" \
  > mere-events-$(date +%F).native
```

For larger ClickHouse deployments use
[`clickhouse-backup`](https://github.com/Altinity/clickhouse-backup) for
incremental, remote-object-store backups. The built-in `BACKUP TABLE …`
statement also works but first needs a backup disk declared in
`config/deploy/clickhouse/config.xml` (none ships by default).

## Restoring from backup

Restores mirror the backups above. **Restore before the instance is taking live
traffic** — bring the accessories up, restore, *then* deploy the app (or keep
`INGEST_DISABLED=true` until the restore completes) so nothing writes over the
data mid-restore.

**Hetzner snapshot (whole disk).** Rebuild the VPS from the snapshot in the
Hetzner Cloud Console (Server → Rebuild from backup). Because both data
directories share the disk, this brings Postgres and ClickHouse back to the same
point in time together — the simplest path, and consistent by construction.

**Logical restore** (from the `pg_dump` / native dumps above):

```bash
# Postgres — restore a custom-format dump. --clean drops the existing objects
# first, so point it at the intended target database.
kamal accessory exec postgres -i \
  "pg_restore -U mere -d mere --clean --if-exists" < mere-pg-2026-06-03.dump

# ClickHouse — the schema must already exist (the app creates it by running
# migrations on boot), so deploy the app first, then stream the rows back in.
kamal accessory exec clickhouse -i \
  "clickhouse-client --query 'INSERT INTO analytics.events_raw_v1 FORMAT Native'" \
  < mere-events-2026-06-03.native
```

A restored Postgres dump already carries the `schema_migrations` bookkeeping, so
the next boot re-runs nothing. The ClickHouse native dump is **data only** — let
the app create the tables via its boot migrations *before* inserting, or the
`INSERT` has no table to target.

## Persistence

Postgres and ClickHouse data live in host volumes under `/data/mere/` on the
VPS. They survive `kamal redeploy`, `kamal app remove`, and image upgrades, and
are lost only if you delete the directory or the VPS itself.

## Troubleshooting

Most failures surface in `kamal logs` and on `GET /healthz` (which returns the
running build `version` and a status body). The recurring ones:

| Symptom | Likely cause | What to do |
|---|---|---|
| `POST /api/v1/ingest/events` returns `503` + `Retry-After: 1` | In-flight buffer hit `INGEST_EVENT_BUFFER` — normal backpressure under a spike. | Transient; clients retry. If sustained, raise `INGEST_EVENT_BUFFER` / `INGEST_FLUSH_EVENTS`, or find why ClickHouse writes are slow. |
| Ingest **and** `/healthz` both return `503` | The ingest **fatal flag**: one flush failed to write to *both* ClickHouse and the Postgres DLQ. | Check ClickHouse and Postgres health in `kamal logs`. The flag clears itself on the next successful write — there is no manual reset. |
| `/healthz` returns `503` but ingest still accepts events | `failed_events` (the DLQ) depth crossed `DLQ_DEPTH_503_THRESHOLD`. | Check depth: `kamal db-console` → `SELECT count(*) FROM failed_events;`. It means CH writes have been failing; fix CH and the drainer empties the DLQ on its own. |
| `failed_events` keeps growing | ClickHouse is unreachable or erroring, so the drainer can't replay. | Fix CH connectivity/disk. The drainer replays `INGEST_DLQ_DRAIN_BATCH_LIMIT` rows per pass once CH is back, then the table shrinks. |
| OAuth login loops, or "invalid issuer" / broken redirects | `OAUTH_ISSUER_URL` doesn't match the externally reachable URL (`proxy.host`). | Set `OAUTH_ISSUER_URL` to exactly `https://<HOSTNAME>` and redeploy. It's advertised in OAuth discovery and signed into redirects, so it must match. |
| Container exits on boot; proxy keeps serving the old version | A migration failed — startup aborts by design, possibly leaving the version **dirty**. | Read `kamal logs` for the failing migration, then recover per [Recovering from a dirty migration](#recovering-from-a-dirty-migration). |
| A query returns fewer rows than expected | A bearer token is bound to one `(user, project)` pair — you may be querying the wrong project's grant. | Confirm the token's project. Tenant isolation injects `project_id` server-side, so another project's data is never visible by design. |
