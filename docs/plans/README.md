# Implementation plans

Forward-looking, test-first implementation plans for individual features, each
small enough to land in one PR. These are working documents: once a plan ships,
its decisions are folded back into [plan.md](../plan.md) (the master design doc
+ decision log) and [architecture.md](../architecture.md) (as-shipped), and the
plan here can be marked `Shipped`.

Distinct from [plan.md](../plan.md) — that is the single authoritative design
record and step-by-step build order. These are scoped per-feature plans for work
that postdates the original build order.

## Plans

| Plan | For | Status |
|---|---|---|
| [ratelimiter-seam.md](ratelimiter-seam.md) | The `RateLimiter` extension seam (no-op default) | Planned |
| [usagesink-seam.md](usagesink-seam.md) | The `UsageSink` extension seam (no-op default) | Planned |

Both seams are defined by the contract in
[ADR-0002](../adr/0002-open-core-hosted-wrapper.md).
