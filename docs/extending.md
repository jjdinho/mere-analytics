# Extending mere

mere ships as a complete product: you run the binary as-is and get the whole
thing. But two concerns deliberately have **no behavior in the open-source
build** — per-tenant rate limiting and usage metering — because what they should
do depends entirely on how you operate the server. Rather than guess, the core
leaves them as **extension seams**: small, stable interfaces with no-op defaults
that you can replace by compiling your own build.

This document explains why those seams exist, the contract each one makes, and
how to build a binary that injects your own implementations — **without forking
the core**, so you keep a clean upgrade path.

If you just want to run mere, you can stop reading: the stock binary needs none
of this. This is for self-hosters who want to add behavior at the two points the
core can't sensibly decide for them.

## Why seams instead of a fork

The obvious way to add a rate limiter would be to edit the request path and
rebuild. The moment you do, you own a fork: every upstream change has to be
re-applied by hand against your edits, and the two drift apart.

mere avoids that by exposing exactly the pieces a wrapper needs as **exported Go
packages**, while keeping everything else under `internal/` (which Go forbids
other modules from importing). You write a tiny separate program that imports the
core as a versioned dependency, supplies your implementations, and starts the
server. Updating is then just bumping the dependency version — the core is
edited in one place, and your additions sit on top of it untouched.

Only two packages are exported, and they are the entire contract you depend on:

| Package | What it gives you |
|---|---|
| [`extension`](../extension/extension.go) | The seam interfaces (`RateLimiter`, `UsageSink`) and their no-op defaults (`AllowAll`, `Discard`). |
| [`app`](../app/app.go) | The composition root: `Build`/`Run`, which wire and start the fully-assembled server and let you inject the seams. |

Everything else — `web`, `auth`, `oauth`, `ingest`, `query`, `clickhouse`,
`postgres`, `config` — stays internal and is free to change between releases.
You never import it directly, so refactors upstream don't break you.

The same two seams are how a separately-run hosted version of mere is built:
pure addition over an unmodified core, no fork. They are generic extension
points, equally available to anyone self-hosting.

## The two seams

Both live in [`extension/extension.go`](../extension/extension.go). Each is one
interface plus a no-op default; the open-source build wires the defaults, so
out of the box there is no rate limiting and nothing is metered.

### `RateLimiter` — allow or deny a request

Consulted on the ingest and query/MCP paths **after the tenant has been
resolved**, so your limiter sees who is calling:

```go
type LimitKey struct {
	Surface   string // "ingest" | "query" | "mcp"
	ProjectID string // resolved tenant; "" before resolution
	UserID    string // bearer surfaces only; "" for ingest
	TokenID   string // opaque credential id, for per-credential limits
	RemoteIP  string
}

type RateLimiter interface {
	// Allow reports whether the request may proceed now. retryAfter is a hint
	// for the 429 Retry-After header when ok is false (zero = omit).
	Allow(ctx context.Context, key LimitKey) (ok bool, retryAfter time.Duration)
}
```

- `Allow` is called on the **request hot path**: it MUST be safe for concurrent
  use and MUST NOT block beyond a small, bounded check.
- It runs *after* `requirePublicToken` (ingest) and `RequireBearer` (query/MCP)
  resolve the tenant, so `ProjectID` is populated and you can limit per project.
- On deny (`ok == false`), the core returns `429` with a `Retry-After` header
  built from your `retryAfter` hint, and the handler never runs.
- The default, `extension.AllowAll`, lets every request through.

Typical use: a token-bucket limiter (in-memory, or Redis-backed if you run more
than one instance) keyed on `ProjectID` or `TokenID` to enforce per-project
quotas or to blunt abusive clients.

### `UsageSink` — observe events that landed

Called once a batch of events has **durably landed in ClickHouse**, so you can
count per-tenant volume (for quotas, dashboards, or billing):

```go
type UsageSink interface {
	RecordIngested(ctx context.Context, projectID string, events int)
}
```

- `RecordIngested` is invoked **off the request hot path** — after the primary
  flush, or, for events that first failed and were queued, after the successful
  dead-letter-queue drain.
- Each event is counted **exactly once**, at its first successful insert.
  Repeated failed drain attempts never count, and an event never passes through
  two successful inserts, so there is no double-count.
- Because it runs off the hot path, your implementation may do real work — but it
  MUST NOT panic and SHOULD NOT block the flusher for long.
- The default, `extension.Discard`, drops every signal.

Typical use: increment a per-project counter in Postgres or a metrics system, or
emit a usage record for invoicing.

## The composition root

[`app`](../app/app.go) is the single entry point that builds the whole server —
config load, database connections, migrations, ClickHouse bootstrap, service
construction, handler assembly — and runs the three-phase SIGTERM shutdown. The
stock `cmd/server/main.go` is just a thin shim over it:

```go
func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := app.Run(context.Background(), app.WithLogger(logger), app.WithVersion(Version)); err != nil {
		logger.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}
```

Your build is the same call plus your injections. The relevant options:

```go
func WithLogger(l *slog.Logger) Option
func WithVersion(v string) Option                                       // build stamp for /healthz
func WithRateLimiter(rl extension.RateLimiter) Option                   // default extension.AllowAll
func WithUsageSink(us extension.UsageSink) Option                       // default extension.Discard
func WithHandlerMiddleware(mw ...func(http.Handler) http.Handler) Option // wrap the outer handler

func Build(ctx context.Context, opts ...Option) (*App, error) // wired, not yet listening
func Run(ctx context.Context, opts ...Option) error           // Build + serve + SIGTERM choreography

func (a *App) Handler() http.Handler // serve it yourself / httptest
func (a *App) Run(ctx context.Context) error
func (a *App) Close() error          // release pools/pipeline for Build-only callers
```

You inherit the real boot sequence and the real shutdown choreography unchanged,
and they stay correct as the core evolves.

`WithHandlerMiddleware` wraps the outer `http.Handler` — useful for cross-cutting
concerns that don't need the resolved tenant (request logging, an edge auth
check, IP-level filtering). Anything that *does* need the tenant belongs in a
seam, which is consulted further in.

### Configuration is unchanged

`app.Build` reads mere's configuration from the **environment**, exactly as the
stock binary does (see the [environment reference](self-host.md#environment-reference)).
The `config` type stays internal; your wrapper sets the same environment
variables and, if it has its own settings (a Redis URL for the limiter, say),
reads them however it likes.

## A worked example

A self-hoster who wants a simple per-project ingest rate limit. Create a small
module that depends on mere:

```go
// cmd/mere-custom/main.go
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/jjdinho/mere-analytics/app"
	"github.com/jjdinho/mere-analytics/extension"
)

// projectLimiter caps each project to a fixed rate on the ingest surface and
// lets everything else through. Replace the body with a real token bucket
// (e.g. golang.org/x/time/rate per project, or a Redis-backed counter if you
// run more than one instance).
type projectLimiter struct{ /* your buckets, mutex, etc. */ }

func (l *projectLimiter) Allow(ctx context.Context, key extension.LimitKey) (bool, time.Duration) {
	if key.Surface != "ingest" {
		return true, 0
	}
	if l.overQuota(key.ProjectID) {
		return false, time.Second // becomes Retry-After: 1 on the 429
	}
	return true, 0
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	err := app.Run(context.Background(),
		app.WithLogger(logger),
		app.WithVersion("mere-custom-1.0.0"),
		app.WithRateLimiter(&projectLimiter{ /* ... */ }),
	)
	if err != nil {
		logger.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}
```

```
go mod init example.com/mere-custom
go get github.com/jjdinho/mere-analytics@<tag>
go build ./cmd/mere-custom
```

Run that binary instead of `mere-server`, with the same environment, and you have
a build with your rate limiter in place. To pick up an upstream release, bump the
`@<tag>` and rebuild — nothing in the core was edited, so there is nothing to
re-apply.

> **AGPL note.** mere is [AGPL-3.0-or-later](../LICENSE). A binary you build that
> links the core is a derivative work: if you offer it to others over a network,
> §13 obliges you to make your modifications' source available to those users.
> Running your own modified build for your own organization carries no such
> obligation. See the [README](../README.md#license).
