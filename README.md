# mere

A small, self-hostable analytics server. One Go binary, two databases
(PostgreSQL + ClickHouse), no SaaS layer. You run it; you own the data.

mere does three things:

1. **Ingest** events from anywhere — a browser snippet, a server SDK, a CLI, an
   agent — over a simple HTTP endpoint.
2. **Store** them in ClickHouse, scoped per project.
3. **Expose** them through a stable, versioned HTTP API, an MCP endpoint (so an
   LLM can query your analytics directly), and a small web UI with a SQL
   playground.

Self-hosters get the whole product. There is no "cloud-only" tier.

## What's in the box

- **Ingest API** — `POST /api/v1/ingest/events`, authenticated by a per-project public
  token. Batches of events land in ClickHouse via an in-process buffered
  pipeline with a Postgres dead-letter queue for transient outages.
- **Query API** — `POST /api/v1/projects/{id}/query` runs arbitrary read-only
  ClickHouse SQL, scoped to one project. Tenant isolation is enforced by
  ClickHouse's `additional_table_filters` setting, not by rewriting your SQL.
- **Schema API** — `GET /api/v1/projects/{id}/schema` returns the queryable
  table/column catalog.
- **MCP endpoint** — `/mcp` exposes `query` and `schema` tools so an MCP client
  (e.g. Claude) can explore a project's analytics.
- **OAuth 2.1 server** — PKCE authorization-code flow issues the bearer tokens
  for the query/schema/MCP surface. Lives in-process at `/oauth/*`.
- **Web UI** — login, teams, projects, invite links, per-project ingest token,
  and a SQL query playground. Server-rendered (templ + htmx), no SPA.

## Quickstart (local)

Requires Go 1.25+ and Docker.

```bash
git clone https://github.com/jjdinho/mere-analytics && cd mere-analytics
./scripts/dev          # brings up Postgres + ClickHouse, runs the server on :8080
```

`scripts/dev` starts the compose stack in `docker/docker-compose.yml` and boots
the server with dev-friendly env (plaintext-cookie mode). There is no public
signup — create the first user against the dev database:

```bash
psql "postgresql://mere:devpass@127.0.0.1:55432/mere" \
  -v email=you@example.com -v password=change-me-please \
  -f scripts/operator/create-user.sql
```

Then open <http://localhost:8080/login>.

## Deploying

mere deploys with [Kamal](https://kamal-deploy.org): a fresh VPS reaches a
working, TLS-terminated deployment (app + Postgres + ClickHouse) from a single
`kamal setup`. You build and push the image from your own machine with
`kamal deploy` — there is no CI image pipeline and no pre-built public image to
pull.

See **[docs/self-host.md](docs/self-host.md)** for the from-zero walkthrough,
the environment reference, operator actions, backups, and migration recovery.

## Documentation

- **[docs/api.md](docs/api.md)** — the public HTTP + MCP API reference.
- **[docs/self-host.md](docs/self-host.md)** — how to run it in production.
- **[docs/architecture.md](docs/architecture.md)** — how it's built and why.
- **[docs/plan.md](docs/plan.md)** — the design plan and decision log.
- **[docs/adr/](docs/adr/)** — architecture decision records (licensing,
  open-core, the extension-seam contract).

## License

[AGPL-3.0-or-later](LICENSE). Self-host, modify, and run mere freely — a
self-hoster's rights are unrestricted. The network-copyleft (AGPL §13) means
anyone who offers a **modified** mere as a network service must share their
modifications. A separately-run hosted version is offered for those who'd rather
not operate it themselves. The licensing and open-core rationale are recorded in
[ADR-0001](docs/adr/0001-adopt-agpl-3.0.md) and
[ADR-0002](docs/adr/0002-open-core-hosted-wrapper.md).
