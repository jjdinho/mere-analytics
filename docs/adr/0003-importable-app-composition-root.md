# 0003 — Expose an importable app-composition root (keep everything else internal)

- **Status:** Accepted
- **Date:** 2026-06-02
- **Related:** [ADR-0002](0002-open-core-hosted-wrapper.md) (this is the
  "packaging prerequisite" called out in its Consequences). The seam *types* it
  references land with [docs/plans/ratelimiter-seam.md](../plans/ratelimiter-seam.md)
  and [docs/plans/usagesink-seam.md](../plans/usagesink-seam.md).

## Context

[ADR-0002](0002-open-core-hosted-wrapper.md) decides the hosted version is a
private `mere-cloud` module that imports this core, injects the `RateLimiter` /
`UsageSink` seams, and serves — never a fork. For that to *compile and run*, the
wrapper needs two things from the core:

1. **To name the seam types** — handled by the exported `extension` package
   (ADR-0002, landed by the seam plans).
2. **To build and start the fully-wired app** — *not yet handled*, and the
   subject of this ADR.

Today all wiring lives in `cmd/server/main.go`'s `run()`: load config → open
Postgres → run migrations → bootstrap ClickHouse + provision the readonly user →
build the query executor, ingest service, auth/oauth/mcp services → assemble
`web.Handler(Options{…})` → start `http.Server` → run the three-phase SIGTERM
choreography. Every package it touches is under `internal/`, which Go forbids
other modules from importing. plan.md states this on purpose: "`internal/`
because nothing here is meant to be imported by other Go modules."

So the wrapper cannot reach `web.Handler`, the service constructors, or the boot
sequence. The options are: export a large chunk of the internals (loses the
boundary), duplicate the wiring in the wrapper (a fork by another name), or
expose **one** composition entry point.

## Decision

**Promote the composition root into a single exported `app` package; keep
everything else `internal/`.**

`mere-cloud` then depends on exactly two exported packages — `app` (build + run)
and `extension` (the seam types) — and nothing else. The `internal/` boundary is
preserved for `web`, `auth`, `oauth`, `ingest`, `query`, `clickhouse`,
`postgres`, `config`, etc.: the wrapper never imports them directly. `app` lives
in the same module, so it is free to import `internal/*`; the wrapper imports
`app`.

### Shape

```go
package app // github.com/jjdinho/mere-analytics/app

// App is a wired, not-yet-listening application: handler + http server + the
// closers for its Postgres/ClickHouse pools and ingest pipeline.
type App struct { /* unexported */ }

type Option func(*options)

func WithLogger(l *slog.Logger) Option
func WithVersion(v string) Option                       // build stamp for /healthz
func WithRateLimiter(rl extension.RateLimiter) Option   // default extension.AllowAll
func WithUsageSink(us extension.UsageSink) Option        // default extension.Discard
func WithHandlerMiddleware(mw ...func(http.Handler) http.Handler) Option // wrap the outer handler

// Build runs the full boot sequence (DB open, migrations, CH bootstrap, service
// construction, handler assembly) and returns a wired App that is not yet
// listening. Config is read from the environment (see below).
func Build(ctx context.Context, opts ...Option) (*App, error)

// Run = Build + ListenAndServe + the three-phase SIGTERM choreography.
func Run(ctx context.Context, opts ...Option) error

func (a *App) Handler() http.Handler        // for serve-it-yourself / httptest callers
func (a *App) Run(ctx context.Context) error
func (a *App) Close() error                 // release pools/pipeline for Build-only callers
```

`cmd/server/main.go` collapses to a shim:

```go
func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := app.Run(context.Background(), app.WithLogger(logger), app.WithVersion(Version)); err != nil {
		logger.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}
```

`mere-cloud`'s own `main` is the same call plus its injections:

```go
app.Run(ctx,
	app.WithVersion(cloudVersion),
	app.WithRateLimiter(redisLimiter),
	app.WithUsageSink(billingMeter),
	app.WithHandlerMiddleware(edgeMiddleware),
)
```

### Config stays internal; the boundary is the environment

`config.Config` remains `internal/`. `app.Build` calls `config.Load()` itself, so
the wrapper does **not** construct an internal type. mere is configured exactly
as it is today — environment variables — and the wrapper, which owns its own
process, sets that environment plus reads its own separate config. If
programmatic config override is ever genuinely needed, add a focused option
later; do not export `config` pre-emptively (YAGNI).

### The boot sequence and SIGTERM choreography move into `app`

The boot order and the three-phase shutdown (ingest disable → HTTP drain →
ingest drain) are *core behavior*, not entry-point glue. Moving them into `app`
means the wrapper inherits them unchanged — a feature, not a cost. `cmd/server`
keeps only its `Version` ldflags var and the shim above.

## Consequences

**Good**

- The exported surface is minimal and intentional: `app` + `extension`. Two
  packages are the entire contract `mere-cloud` depends on; everything else stays
  internal and freely refactorable.
- No duplicated wiring, so no fork. The wrapper gets the real boot sequence and
  the real SIGTERM choreography for free, and stays correct as they evolve.
- `App.Handler()` also gives the existing in-repo HTTP integration tests a
  cleaner construction path if they want it (optional; not required by this ADR).

**Costs / risks**

- **`app` is now public API.** Changing `Build`/`Run`/the options is a breaking
  change for `mere-cloud`, on the same footing as the seam contract. Keep the
  surface small for exactly this reason; resist adding options speculatively.
- **`Version` plumbing moves.** It's passed via `WithVersion` instead of read
  from a `cmd/server` package var inside the handler. The Dockerfile's
  `-ldflags "-X main.Version=…"` target is unchanged (it still stamps
  `cmd/server`); `main` forwards it through the option. The wrapper stamps its
  own version the same way.
- **One-time refactor risk.** ~160 lines of wiring move packages. It must be
  behavior-preserving (below).

## How it lands (behavior-preserving)

This is a refactor, so the rule is *tests green before and after* — no behavior
change, no new feature. Suggested order:

1. Create `app/` with `options` + `Option` constructors and the `extension`
   defaults (`AllowAll`, `Discard`) applied when unset. (Depends on the
   `extension` package from the seam plans; can land before the seams are
   *wired*, since the defaults are no-ops.)
2. Move `run()`'s body from `cmd/server/main.go` into `app.Build` + `App.Run`
   verbatim — same boot order, same SIGTERM phases, same timeouts. Thread the
   logger/version/seam options through to `web.Options` and `ingest.Options`.
3. Reduce `cmd/server/main.go` to the shim. Keep the `Version` ldflags var.
4. Green gates: the existing `e2e/` boot test, `internal/web` integration
   tests, and the ingest shutdown/DLQ tests must all pass unchanged. Add one
   thin `app`-level test that `Build` returns a serving handler and `Close`
   releases cleanly.

No `mere-cloud` code is required to land this — the core simply becomes
*consumable*. The wrapper is built afterward, against the now-stable `app` +
`extension` surface.

## Alternatives

- **Move the internal packages to `pkg/`** (export them). Rejected: exposes a
  large, unstable API and discards the `internal/` discipline plan.md chose
  deliberately. We want to export the *composition*, not the components.
- **Duplicate the wiring in `mere-cloud`.** Rejected: it is a fork of the boot
  sequence; it drifts from the core on every change — the exact failure
  [ADR-0002](0002-open-core-hosted-wrapper.md) exists to prevent.
- **Sidecar-only, no import** (run the stock core image, do everything in a
  proxy). Already weighed in [ADR-0002](0002-open-core-hosted-wrapper.md): fine
  for edge concerns, but the in-process seams need the core's resolved tenant and
  its ingest pipeline, which a proxy can't supply. This ADR is what makes the
  in-process path possible; the two compose.
- **Export `config` so the wrapper builds a `Config` and passes it in.**
  Rejected for now: enlarges the surface for no need the environment doesn't
  already meet. Reconsider only if a concrete programmatic-override requirement
  appears.
