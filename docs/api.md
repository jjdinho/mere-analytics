# mere — API reference

The public HTTP surface and the MCP endpoint. Everything here is grounded in the
shipped implementation; where this doc and `docs/plan.md` disagree, this doc
(and the code) win.

## Conventions

- **Base URL** — whatever host you deploy behind (`https://analytics.example.com`).
  All paths below are relative to it.
- **Versioning** — `/v1` is forever-stable. Breaking changes ship at `/v2`;
  additive changes (new fields, new endpoints) are allowed within `/v1`.
- **Two auth planes:**
  - **Public ingest token** — a per-project `mere_pub_…` token, sent as
    `Authorization: Bearer mere_pub_…`. Non-secret by design (it lives in client
    HTML). Resolves the project; the project is never taken from the URL or body.
  - **OAuth 2.1 bearer token** — a short-lived access token issued by the
    in-process OAuth server (`/oauth/*`), sent as `Authorization: Bearer …`.
    Bound to one `(user, project)` pair. Protects `/api/v1/*`, `/mcp`, and
    `/api/v1/whoami`.
- **CORS** — `/api/v1/ingest/events`, `/api/v1/*`, and `/mcp` answer cross-origin requests.
  With no `ALLOWED_ORIGINS` configured the server returns
  `Access-Control-Allow-Origin: *`; with a configured allowlist it echoes a
  matching `Origin` (and sets `Vary: Origin`) or omits CORS headers entirely.
  Allowed methods: `GET, POST, OPTIONS`. Allowed headers:
  `Authorization, Content-Type`. Preflight `OPTIONS` returns `204`.

---

## Ingest

### `POST /api/v1/ingest/events`

Submit a batch of events. Authenticated with the project's **public ingest
token**.

```
Authorization: Bearer mere_pub_<token>
Content-Type: application/json
```

**Request body** — a single object with an `events` array:

```json
{
  "events": [
    {
      "event": "pageview",
      "timestamp": "2026-06-02T14:30:45.123Z",
      "distinct_id": "user-123",
      "session_id": "sess-456",
      "properties": { "path": "/pricing", "ref": "twitter" },
      "plan_tier": "pro"
    }
  ]
}
```

Per-event fields:

| Field | Required | Type | Notes |
|---|---|---|---|
| `event` | **yes** | string | Must be non-empty. |
| `timestamp` | **yes** | string \| number | ISO 8601 / RFC 3339 string (e.g. `2026-06-02T14:30:45Z`), **or** a number of epoch milliseconds (e.g. `1717200000000`). |
| `distinct_id` | no | string | Caller-supplied identity. Stored `NULL` if omitted. |
| `session_id` | no | string | |
| `properties` | no | object | Arbitrary JSON; stored verbatim. Defaults to `{}`. |
| *(any other field)* | no | any | Folded verbatim into the event's `extras` column — see below. |

> **Lenient on extras.** Any top-level field on an event that isn't one of the
> first-class fields above is collected verbatim into that event's `extras`
> column — there's no rejection and no migration to ship, so consumers can
> attach and later query their own fields freely. You may also send an explicit
> `extras` object; stray fields merge on top of it. (Unknown keys on the
> *request envelope* itself — anything other than `events` — are still rejected
> with `400`.)

**Responses:**

| Status | When | Body |
|---|---|---|
| `202 Accepted` | At least one event passed validation and was enqueued. | `{"accepted": N, "rejected": M, "errors": [...]}` |
| `200 OK` | The batch contained zero valid events (e.g. empty array, or every event rejected). | `{"accepted": 0, "rejected": M, "errors": [...]}` |
| `400 Bad Request` | Malformed JSON, an unparseable timestamp string, or an unknown key on the request envelope. | `invalid json: <detail>` |
| `401 Unauthorized` | Missing/non-`mere_pub_` token, or unknown / revoked token, or the project is soft-deleted. | `unauthorized` (+ `WWW-Authenticate: Bearer realm="api", error="invalid_request"\|"invalid_token"`) |
| `413 Payload Too Large` | Body exceeds `INGEST_MAX_BODY_BYTES` (default 10 MiB). | `request body too large` |
| `503 Service Unavailable` | Ingest disabled / pipeline fatal / buffer saturated (see below). | `ingest disabled` \| `ingest down` \| `ingest channel full` (+ `Retry-After`) |
| `500 Internal Server Error` | Infrastructure failure during token lookup or submit. | `internal server error` |

Validation never silently drops: rejected events are reported per-index in
`errors`, while the rest of the batch is accepted.

```json
{
  "accepted": 2,
  "rejected": 1,
  "errors": [
    { "index": 1, "reason": "event required" }
  ]
}
```

`reason` is one of `"event required"` or `"timestamp required"`.

**`503` variants** (the producer should retry — events are never dropped):

| Body | Cause | `Retry-After` |
|---|---|---|
| `ingest disabled` | `INGEST_DISABLED=true` kill switch. | `300` |
| `ingest down` | Both ClickHouse and the Postgres DLQ failed in the same flush (fatal state). Clears on the first successful flush. | `5` |
| `ingest channel full` | In-flight event buffer saturated (`INGEST_EVENT_BUFFER`). | `1` |

---

## Query

### `POST /api/v1/projects/{project_id}/query`

Run read-only ClickHouse SQL scoped to one project. **OAuth bearer token
required.** The token's granted project must equal `{project_id}` — any
mismatch, unknown project, soft-deleted project, or project you can't see
returns `404` (existence is never confirmed to an unauthorized caller).

```
Authorization: Bearer <oauth-access-token>
Content-Type: application/json
```

```json
{ "sql": "SELECT event, count() AS n FROM events_raw_v1 GROUP BY event ORDER BY n DESC LIMIT 20" }
```

**Success — `200`**, streamed as a JSON envelope:

```json
{
  "columns": [
    { "name": "event", "type": "LowCardinality(String)" },
    { "name": "n", "type": "UInt64" }
  ],
  "rows": [
    ["pageview", 1420],
    ["click", 318]
  ],
  "stats": { "rows": 2, "elapsed_ms": 12 }
}
```

- `columns` — ClickHouse column name + declared type for each result column.
- `rows` — arrays of values in column order. `NULL` → `null`, timestamps →
  RFC 3339 nanosecond strings, UUIDs → 36-char strings.
- `stats` — `rows` returned and wall-clock `elapsed_ms`.

**Tenant isolation.** Your SQL is sent to ClickHouse **unmodified**. The server
attaches `additional_table_filters` for `analytics.events_raw_v1` so ClickHouse
transparently injects `project_id = '<your-project>'` into every reference to
the events table — including self-joins, subqueries, and aliases. The query runs
as the read-only `mere_readonly` user, which cannot reach `system.*` or any
non-allowlisted table.

**Per-request limits** (applied server-side, not overridable from your SQL):

| Setting | Value |
|---|---|
| `max_execution_time` | 30s |
| `max_memory_usage` | 4 GiB |
| `max_result_rows` | 1,000,000 |

**Errors:**

| Status | When | Body |
|---|---|---|
| `400` | Missing/empty `sql`. | `sql is required` |
| `400` | Invalid JSON. | `invalid json: <detail>` |
| `400` | Any ClickHouse error — parse error, timeout, out-of-memory. | The verbatim ClickHouse message. |
| `401` | Missing / invalid / expired bearer token. | `unauthorized` |
| `404` | Project mismatch / unknown / soft-deleted / not visible. | (standard 404) |
| `413` | Body exceeds `QUERY_MAX_BODY_BYTES` (default 256 KiB). | `request body too large` |

ClickHouse errors are returned verbatim on purpose — `mere_readonly`'s grants
already bound what's observable, and power users want the real message.

---

## Schema

### `GET /api/v1/projects/{project_id}/schema`

The queryable table/column catalog for a project. Same auth and `404` rules as
the query endpoint.

**Success — `200`:**

```json
{
  "tables": [
    {
      "name": "events_raw_v1",
      "columns": [
        { "name": "project_id",  "type": "UUID" },
        { "name": "event",       "type": "LowCardinality(String)" },
        { "name": "distinct_id", "type": "Nullable(String)" },
        { "name": "timestamp",   "type": "DateTime64(3, 'UTC')" },
        { "name": "session_id",  "type": "Nullable(String)" },
        { "name": "properties",  "type": "String" },
        { "name": "extras",      "type": "String" },
        { "name": "received_at", "type": "DateTime64(3, 'UTC')" }
      ]
    }
  ]
}
```

`events_raw_v1` is the only queryable table today; the schema endpoint and the
query executor share one allowlist, so it cannot drift.

### The `events_raw_v1` table

| Column | Type | Notes |
|---|---|---|
| `project_id` | `UUID` | Filtered automatically — you never see other projects' rows. |
| `event` | `LowCardinality(String)` | The event name. |
| `distinct_id` | `Nullable(String)` | Caller-supplied identity, or `NULL`. |
| `timestamp` | `DateTime64(3, 'UTC')` | Event time supplied at ingest. |
| `session_id` | `Nullable(String)` | |
| `properties` | `String` | JSON text. Query with `JSONExtract*`, e.g. `JSONExtractString(properties, 'path')`. |
| `extras` | `String` | JSON text, same querying approach. |
| `received_at` | `DateTime64(3, 'UTC')` | Server receive time (default `now64`). |

The table is `ORDER BY (project_id, timestamp, event)` and partitioned
`toYYYYMM(timestamp)`. `properties` and `extras` are stored as JSON **strings**,
not as a native JSON/Map type — use ClickHouse's `JSONExtract*` functions to
read fields out of them.

---

## OAuth 2.1 (issuing bearer tokens)

The query/schema/MCP surface is protected by OAuth 2.1 access tokens. The flow
is **authorization-code + PKCE only** (public clients, no client secret, no
refresh tokens). When an access token expires (1 hour), re-run the flow.

### Discovery — `GET /.well-known/oauth-authorization-server`

```json
{
  "issuer": "https://analytics.example.com",
  "authorization_endpoint": "https://analytics.example.com/oauth/authorize",
  "token_endpoint": "https://analytics.example.com/oauth/token",
  "registration_endpoint": "https://analytics.example.com/oauth/register",
  "response_types_supported": ["code"],
  "grant_types_supported": ["authorization_code"],
  "code_challenge_methods_supported": ["S256"],
  "token_endpoint_auth_methods_supported": ["none"],
  "scopes_supported": ["api"]
}
```

### Register a client — `POST /oauth/register` (RFC 7591)

```json
{ "client_name": "my-cli", "redirect_uris": ["http://127.0.0.1:8765/callback"] }
```

`redirect_uris` is required. Allowed: any `https://` URI, or `http://` only for
`localhost` / `127.0.0.1` loopback. Fragments are rejected.

**`201`:**

```json
{
  "client_id": "client_…",
  "client_name": "my-cli",
  "redirect_uris": ["http://127.0.0.1:8765/callback"],
  "grant_types": ["authorization_code"],
  "response_types": ["code"],
  "token_endpoint_auth_method": "none"
}
```

Errors return `400 {"error": "invalid_client_metadata", "error_description": "…"}`.

### Authorize — `GET /oauth/authorize`

Send the user's browser here:

```
/oauth/authorize?response_type=code
  &client_id=client_…
  &redirect_uri=http://127.0.0.1:8765/callback
  &scope=api
  &state=<opaque>
  &code_challenge=<base64url(sha256(verifier))>
  &code_challenge_method=S256
```

- If the user has no web-UI session → `303` to `/login?next=…`; after login they
  return here.
- If signed in → a consent page where they pick **which project** to grant.
- Invalid `client_id` / `redirect_uri` → `400` (the redirect target can't be
  trusted). Other invalid params → `303` back to `redirect_uri?error=…&state=…`
  (`unsupported_response_type`, `invalid_request`, `invalid_scope`).

On approval (`POST /oauth/authorize`, CSRF-protected) the browser is redirected
to `redirect_uri?code=<code>&state=<state>`. On denial,
`?error=access_denied&state=…`. Authorization codes are one-shot and expire in
10 minutes.

### Token — `POST /oauth/token`

`application/x-www-form-urlencoded`:

```
grant_type=authorization_code
code=<code>
redirect_uri=http://127.0.0.1:8765/callback
client_id=client_…
code_verifier=<original PKCE verifier>
```

**`200`** (`Cache-Control: no-store`):

```json
{ "access_token": "…", "token_type": "Bearer", "expires_in": 3600, "scope": "api" }
```

Errors return `400` with an RFC 6749 body, e.g.
`{"error": "invalid_grant", "error_description": "code is invalid or already used"}`
or `{"error": "unsupported_grant_type", …}`. There are no refresh tokens.

### `GET /api/v1/whoami`

A bearer-protected smoke endpoint that echoes the token's grant:

```json
{ "user_id": "…", "project_id": "…", "client_id": "client_…", "scope": "api" }
```

---

## MCP

### `/mcp`

A [Model Context Protocol](https://modelcontextprotocol.io) endpoint over the
Streamable HTTP transport (stateless). Authentication is the **same OAuth bearer
token** as `/api/v1/*`; the project is taken from the token's grant, so MCP
tools take no `project_id` argument. CORS matches the rest of the API surface.

Two tools:

| Tool | Arguments | Returns |
|---|---|---|
| `query` | `sql` (string, required) | `{"columns":[{"name","type"}], "rows":[[…]], "stats":{"rows","elapsed_ms"}}` — same envelope as the HTTP query API. Results are capped at 1000 rows; always add a `LIMIT`. |
| `schema` | none | `{"tables":[{"name","columns":[{"name","type"}]}]}` — the queryable catalog. |

Both tools enforce the same tenant isolation as the HTTP API and re-check
project visibility on each call (a project soft-deleted within the token's
lifetime denies with a `project not found` tool error).

**Error model:** ClickHouse errors (parse / timeout / memory), an empty `sql`,
or breaching the row cap come back as **tool errors** (`isError: true`) carrying
the verbatim message, so the model can read and self-correct. Only a handler
panic or an infrastructure failure (e.g. Postgres down during the visibility
check) becomes a JSON-RPC `internal_error` (`-32603`).

---

## Health

### `GET /healthz`

Unauthenticated. Returns `200` when healthy, `503` when the ingest pipeline is
in fatal state or the DLQ depth has crossed `DLQ_DEPTH_503_THRESHOLD` (so
`kamal-proxy` circuit-breaks the instance).

```json
{ "status": "ok", "version": "v0.1.0", "ingest_disabled": false, "dlq_depth": 0 }
```
