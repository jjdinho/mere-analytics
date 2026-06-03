# Plan — `RateLimiter` extension seam

- **Status:** Implemented — `extension.RateLimiter`/`AllowAll` + the
  `internal/web/ratelimit.go` middleware, wired (after tenant resolution) on the
  ingest, query, and MCP chains in `internal/web/server.go`.
- **Contract:** [ADR-0002](../adr/0002-open-core-hosted-wrapper.md) · License:
  [ADR-0001](../adr/0001-adopt-agpl-3.0.md)
- **Supersedes the design portion of:** the "Per-project ingest rate limit" TODO
  in [TODOS.md](../../TODOS.md).

## Goal

Add a `RateLimiter` seam to the core so a hosted build (or a self-hoster) can
enforce per-tenant request limits **without modifying the core**. The core ships
the no-op `AllowAll` default, so the open-source product's behavior is byte-for-byte
unchanged: there is still no rate limiting in this repo, only a generic
extension point.

Non-goal: the actual limiter (Redis token bucket, plan-aware quotas). That lives
in the private `mere-cloud` wrapper. This plan lands the *interface and the
honored call sites*, nothing more.

## What changes (surface)

1. **New exported package `extension/`** (top-level, not under `internal/` — it
   must be importable by `mere-cloud`). Holds `RateLimiter`, `LimitKey`,
   `AllowAll`. Per ADR-0002 this is deliberately tiny: an interface + a no-op
   struct, no behavior.
2. **`internal/web/ratelimit.go`** (new) — a `rateLimit(...)` middleware
   (`func(http.Handler) http.Handler`, the existing middleware shape) that builds
   a `LimitKey`, calls `limiter.Allow`, and on deny writes `429` + `Retry-After`.
3. **`internal/web/server.go`** — `Options` gains
   `RateLimiter extension.RateLimiter`; `Handler` defaults a nil value to
   `extension.AllowAll{}` and mounts the middleware on the ingest and query/MCP
   chains.

## The interface (recap from ADR-0002)

```go
package extension

type LimitKey struct {
	Surface   string // "ingest" | "query" | "mcp"
	ProjectID string // resolved tenant; "" before resolution
	UserID    string // bearer surfaces only; "" for ingest
	TokenID   string // opaque credential id, for per-credential limits
	RemoteIP  string
}

type RateLimiter interface {
	Allow(ctx context.Context, key LimitKey) (ok bool, retryAfter time.Duration)
}

type AllowAll struct{}
func (AllowAll) Allow(context.Context, LimitKey) (bool, time.Duration) { return true, 0 }
```

## Where it wires (exact sites)

The limiter must run **after** the tenant is resolved, so `LimitKey.ProjectID`
is populated:

- **Ingest** — after `requirePublicToken` (which stashes the project via
  `projectFromCtx`, `internal/web/ingest_handlers.go`), before `postIngest`. In
  `server.go` the chain
  `cors(MaxBody(requirePublicToken(postIngest)))` becomes
  `cors(MaxBody(requirePublicToken(rateLimit(lim,"ingest")(postIngest))))`.
  Key from `projectFromCtx(ctx)` + `r.RemoteAddr`.
- **Query / MCP** — after `RequireBearer` (which stashes the grant via
  `oauth.ContextWith`; read it back with `oauth.FromContext`,
  `internal/web/oauth_middleware.go`). Key from the `AccessContext`'s
  `ProjectID` + `UserID` + `r.RemoteAddr`, `Surface` `"query"` / `"mcp"`.

On deny: `429 Too Many Requests`; set `Retry-After` to `ceil(retryAfter
seconds)` when `retryAfter > 0` (omit otherwise). This mirrors the existing
saturation responses (`503` + `Retry-After`) so clients already handle the
shape. The wrapped handler does **not** run.

## TDD steps (red → green)

Per the repo's red/green rule, write each test, watch it fail, then implement.
Tests live in `internal/web/ratelimit_test.go` and reuse the existing
`internal/web` HTTP-integration harness (see `ingest_integration_test.go`,
`server_test.go`). The limiter is faked, not real — a `stubLimiter` whose
`Allow` returns a programmed verdict and records the `LimitKey` it saw.

| # | Test (red) | Asserts |
|---|---|---|
| 1 | Default `Options` (nil `RateLimiter`) → `POST /api/v1/ingest/events` with a valid token | Still `202`. Proves the no-op default leaves behavior unchanged (`Handler` substituted `AllowAll`). |
| 2 | Injected `stubLimiter{ok: false, retryAfter: 2s}` → ingest POST | `429`; `Retry-After: 2`; `postIngest` never ran (assert via a sentinel — e.g. nothing submitted / a spy next-handler not hit). |
| 3 | Injected `stubLimiter{ok: true}` → ingest POST | `202`; the captured `LimitKey` has `Surface:"ingest"`, `ProjectID` = the token's project, `RemoteIP` set, `UserID:""`. |
| 4 | `stubLimiter{ok:false, retryAfter:0}` → ingest POST | `429`; **no** `Retry-After` header (zero hint omitted). |
| 5 | Injected `stubLimiter{ok:false}` → query path `POST /api/v1/projects/{id}/query` with a valid bearer | `429`; the query executor is never called; captured key has `Surface:"query"`, `ProjectID`/`UserID` from the grant. |
| 6 | `stubLimiter{ok:true}` records the key once per request | Limiter consulted exactly once per request (not per middleware re-entry). |

Then implement `extension` + `rateLimit` + the `server.go` wiring until green.
Keep the existing ingest/query/MCP tests green — the default path must not change.

## Edge cases / contract notes

- **Concurrency.** `Allow` may be called from many goroutines; the core makes no
  serialization promise. (`AllowAll` is trivially safe.)
- **Hot path.** `Allow` is on the request path; the contract says it must be a
  small bounded check, not a blocking network round-trip without its own timeout.
  The core does not enforce this — it is a documented expectation on the impl.
- **Order vs. body limits.** The limiter sits inside `MaxBody`, so an
  oversized-body `413` still short-circuits first; a denied request is rejected
  before the handler reads/parses the body.
- **No new leak.** The request has already passed auth when the limiter runs, so
  a `429` reveals nothing a `202`/`200` wouldn't.

## Definition of done

- `extension` package exists, exported, with `RateLimiter` + `LimitKey` +
  `AllowAll`; doc comments match ADR-0002.
- `web.Options.RateLimiter` wired; nil → `AllowAll`; mounted on ingest +
  query + MCP chains after tenant resolution.
- Tests 1–6 green; full suite green; default behavior unchanged.
- `docs/architecture.md` "Request layers" gains one line for the seam (as-shipped);
  the TODOS.md "Per-project ingest rate limit" entry is updated to point here.
