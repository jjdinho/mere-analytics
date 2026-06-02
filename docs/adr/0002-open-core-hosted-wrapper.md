# 0002 — Open-core: a hosted wrapper around an unmodified core, via extension seams

- **Status:** Accepted
- **Date:** 2026-06-02
- **Related:** [ADR-0001](0001-adopt-agpl-3.0.md) (license). Implemented by
  [docs/plans/ratelimiter-seam.md](../plans/ratelimiter-seam.md) and
  [docs/plans/usagesink-seam.md](../plans/usagesink-seam.md).

## Context

We want to sell a **hosted** mere — same software, run for the customer, billed
monthly — while keeping this repo a complete, self-hostable open-source product
(no cloud-only tier; [ADR-0001](0001-adopt-agpl-3.0.md)). The hosted version
needs things the self-host product deliberately does **not** have (see plan.md
non-goals): self-serve signup, billing, per-tenant rate limiting, usage
metering for invoicing, and automated provisioning.

The trap every open-core project must avoid is **maintaining the product in two
places**. That happens when you *fork* the core to bolt commercial features on.
From then on, every core change has to be ported across the fork, and the two
drift. We will not fork.

mere is unusually well-positioned to avoid forking, because the seams already
exist in the code:

- The whole HTTP surface is one composable unit: `web.Handler(opts Options)
  http.Handler` (`internal/web/server.go`).
- Middleware is already `func(http.Handler) http.Handler` (`CORS`, `MaxBody`,
  `RequireBearer`). A rate-limit or metering hook is the *same shape*.
- The app is already multi-tenant (teams/projects; ClickHouse
  `additional_table_filters`). "Many customers on one cluster" is the model it
  was built for.
- There is deliberately **no public signup** — signup is a hosted concern, and
  it already lives outside the core.

## Decision

**1. The core never knows the hosted version exists. The hosted layer is pure
addition, lives in a separate private repo, and consumes the core as a
versioned artifact.** Edits happen in the core, in one place; the wrapper only
adds. Updates flow one direction: core releases a tagged version → the wrapper
bumps the dependency on its own schedule. If we ever find ourselves editing a
core file *to make hosting work*, we have failed this decision and must instead
turn that need into a generic seam (below).

**2. Two repos.**

- `mere-analytics` (this repo, public, AGPL-3.0) — the whole product, plus a
  small, stable set of **extension seams** with no-op defaults.
- `mere-cloud` (private) — `require github.com/jjdinho/mere-analytics vX.Y.Z`,
  pinned. Contains the proprietary hosted layer (below).

**3. The proprietary hosted layer splits along a process boundary.**

- **Control plane — a separate service.** Marketing/signup, Stripe checkout +
  webhooks, plan/subscription/quota state, the customer dashboard, and
  provisioning (on signup, create a team + project in mere). This talks to mere
  over its existing HTTP API and Postgres. It is a *different process*, so it
  does not link the core at all — naturally separate, no coupling, and cleanly
  outside the core's license (see "AGPL boundary" below). **Most hosted features
  live here** and need zero core changes.
- **Hosted binary — in-process, thin.** Its own `main` that builds the core
  app, injects real implementations of the seams, and serves. This is the *only*
  proprietary code that runs in-process with the core, and it stays small:
  ideally just the two seam implementations plus wiring.

**4. The core exposes exactly two in-process extension seams**, because those
are the only hosted concerns that must sit *inside* the request/ingest path:

- `RateLimiter` — consulted on the ingest and query/MCP paths, after the tenant
  is resolved, to allow/deny per plan. Default: **allow-all** (no-op).
- `UsageSink` — called by the ingest pipeline after a batch durably lands, with
  `(projectID, eventCount)`, so the hosted layer can meter for billing. Default:
  **discard** (no-op).

Everything else (signup, billing, dashboard, provisioning) does **not** get a
seam — it lives in the separate control plane. We add a seam only when a hosted
concern genuinely cannot be done from outside the process.

**5. Seam types live in an exported package so a separate module can inject
them.** Today every package is under `internal/`, which Go forbids other modules
from importing (noted in plan.md: "`internal/` because nothing here is meant to
be imported"). To let `mere-cloud` supply implementations, the seam
*interfaces and their no-op defaults* move into a new exported package,
`github.com/jjdinho/mere-analytics/extension`. `web.Options` and
`ingest.Options` reference `extension.RateLimiter` / `extension.UsageSink`. The
rest of the app stays `internal/`. This is the single, deliberately tiny
concession to the all-internal stance: an interface plus a no-op struct, no
behavior. It also makes the core genuinely *extensible* for self-hosters, who
can compile their own build with a custom limiter or usage sink.

## The seam contract

This is the promise the core makes to any wrapper. It is a stable API: changing
it is a breaking change for `mere-cloud` and is treated like any other `/v1`
break.

Package `extension` (exported):

```go
package extension

import (
	"context"
	"time"
)

// LimitKey identifies what is being rate-limited. Fields are populated at the
// point in the chain where identity is known; a limiter uses whichever it needs.
// ProjectID is set once the public ingest token or OAuth grant has resolved.
type LimitKey struct {
	Surface   string // "ingest" | "query" | "mcp"
	ProjectID string // resolved tenant; "" before resolution
	UserID    string // bearer surfaces only; "" for ingest
	TokenID   string // opaque credential id, for per-credential limits
	RemoteIP  string
}

// RateLimiter decides whether a request may proceed. The core ships AllowAll.
// Allow MUST be safe for concurrent use and MUST NOT block on the hot path
// beyond a small bounded check.
type RateLimiter interface {
	// Allow reports whether the request may proceed now. retryAfter is a hint
	// for the 429 Retry-After header when ok is false (zero = omit).
	Allow(ctx context.Context, key LimitKey) (ok bool, retryAfter time.Duration)
}

// AllowAll is the no-op default: every request proceeds.
type AllowAll struct{}

func (AllowAll) Allow(context.Context, LimitKey) (bool, time.Duration) { return true, 0 }

// UsageSink receives a usage signal each time the ingest pipeline durably
// accepts events for a project. The core ships Discard. RecordIngested is
// called off the request hot path (after the batch lands in ClickHouse), so a
// hosted implementation may do real work, but MUST NOT panic and SHOULD NOT
// block the flusher for long.
type UsageSink interface {
	RecordIngested(ctx context.Context, projectID string, events int)
}

// Discard is the no-op default: usage signals are dropped.
type Discard struct{}

func (Discard) RecordIngested(context.Context, string, int) {}
```

Wiring contract:

- `web.Options.RateLimiter extension.RateLimiter` — nil/zero defaults to
  `AllowAll`. The limiter is consulted **after** `requirePublicToken` (ingest)
  and `RequireBearer` (query/MCP) resolve the tenant, so `LimitKey.ProjectID` is
  populated. On deny: `429` + `Retry-After`, consistent with the existing
  saturation `503` shape.
- `ingest.Options.UsageSink extension.UsageSink` — nil/zero defaults to
  `Discard`. `RecordIngested` is invoked after events are acknowledged by
  ClickHouse — at the primary flush, or (for events that first failed and were
  queued) at the successful DLQ drain. Each event is counted **exactly once**, at
  its first successful insert; repeated failed drain attempts never count, and an
  event never passes through two successful inserts, so there is no double-count.

The two seams are landed independently and test-first; see the plan docs linked
above.

## Consequences

**Good**

- One source of truth. Core changes are made once; `mere-cloud` picks them up by
  bumping a version. No port-across-fork tax.
- The proprietary surface is tiny and well-bounded: a separate control-plane
  process + two seam implementations. Easy to reason about, easy to keep closed.
- The seams make the OSS product more extensible (self-hosters benefit), so they
  earn their place in the open repo rather than being commercial-only scaffolding.

**Costs / follow-ups**

- **Packaging prerequisite.** For `mere-cloud` to *build and start* the app
  (not just name the seam types), the composition root must be importable too.
  This is its own decision — see [ADR-0003](0003-importable-app-composition-root.md):
  promote the wiring in `cmd/server/main.go` into a thin importable `app` package
  so the wrapper's `main` can compose it and inject the seams, leaving
  `cmd/server/main.go` a few-line shim. Not required to land the seams themselves
  (they default to no-op in-tree); required before the wrapper exists.
- **The seam contract is now API.** Renaming a field or changing a signature is
  a breaking change for the wrapper. Keep the surface minimal for exactly this
  reason; resist adding seams speculatively.
- **plan.md non-goal needs a footnote.** "No SaaS overlay … no rate limiting" in
  the repo remains true in spirit — there is no SaaS *logic* here. What the repo
  now ships is generic, no-op *extension points*. plan.md is updated to say so.

**AGPL boundary (see [ADR-0001](0001-adopt-agpl-3.0.md))**

- The **control plane** is a separate process talking to mere over HTTP/Postgres
  — no linking, so AGPL copyleft does not reach it. This is the clean boundary
  and is where most proprietary code should live.
- The **hosted binary** does link the core (it imports `extension` and the app
  builder). That combined work would normally be AGPL; it stays proprietary only
  because **we hold the core's copyright** and dual-license it to ourselves. This
  is exactly why ADR-0001's CLA requirement is load-bearing: keep that in-process
  proprietary surface thin, and never let un-CLA'd contributions into the core,
  or the dual-licensing right erodes.

## Alternatives

- **Fork the core for hosting.** Rejected — this *is* the two-places problem the
  decision exists to prevent.
- **Reverse proxy / sidecar in front of an unmodified core image** (no Go
  import). Genuinely decoupled and great for edge concerns (TLS, IP rate limits,
  WAF). Rejected as the *primary* mechanism because the two seams need
  in-process context: plan-aware limiting needs mere's resolved project id, and
  accurate per-tenant event metering should come from the pipeline that actually
  wrote to ClickHouse, not from a proxy re-parsing request bodies. A proxy
  remains a fine *complement* at the edge.
- **MIT core + proprietary `ee/` directory in this repo** (PostHog/Cal.com).
  Rejected with [ADR-0001](0001-adopt-agpl-3.0.md): keeps the open repo 100%
  open instead of mixing licenses by directory.
- **Per-customer isolated deployments** (control plane spins up a mere stack per
  customer) instead of shared multi-tenancy. Deferred — heavier ops; mere's
  app-layer isolation already supports the shared model. Revisit only for
  customers who contractually require single-tenant isolation.
