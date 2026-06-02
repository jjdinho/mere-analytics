# Plan — `UsageSink` extension seam

- **Status:** Planned
- **Contract:** [ADR-0002](../adr/0002-open-core-hosted-wrapper.md) · License:
  [ADR-0001](../adr/0001-adopt-agpl-3.0.md)

## Goal

Add a `UsageSink` seam so a hosted build can **meter per-tenant event volume for
billing** without modifying the core, and without a proxy re-parsing request
bodies — the count comes from the pipeline that actually wrote to ClickHouse.
The core ships the no-op `Discard` default, so the open-source product is
unchanged: it counts nothing and bills nothing.

Non-goal: the aggregation/billing implementation (buffering, Stripe metered
usage). That lives in the private `mere-cloud` wrapper.

## What changes (surface)

1. **`extension/` package** (the same exported package introduced by the
   [RateLimiter seam](ratelimiter-seam.md)) gains `UsageSink` + `Discard`.
2. **`internal/ingest/ingest.go`** — `Options` gains
   `UsageSink extension.UsageSink`; `NewService` defaults a nil value to
   `extension.Discard{}` and stores it on `Service`.
3. **`internal/ingest/flusher.go`** + **`internal/ingest/dlq.go`** — call
   `RecordIngested` after a successful ClickHouse insert.

## The interface (recap from ADR-0002)

```go
package extension

type UsageSink interface {
	RecordIngested(ctx context.Context, projectID string, events int)
}

type Discard struct{}
func (Discard) RecordIngested(context.Context, string, int) {}
```

## Where it wires (exact sites)

A batch can mix projects (`flushItem.projectID` is per-event,
`internal/ingest/flusher.go`), so the call site **groups items by project** and
emits one `RecordIngested(projectID, count)` per distinct project.

- **Primary flush** — `attemptFlush` (`flusher.go`), in the
  `insertIntoClickHouse(...) == nil` success branch (alongside the existing
  fatal-flag clear). These events have just durably landed.
- **DLQ drain** — the successful-replay branch in `dlq.go` (where a
  `failed_events` row is replayed into ClickHouse and then
  `DeleteFailedEvent`-d). These are events that failed the primary flush and are
  landing durably for the first time now.

**Counting rule (exactly once).** An event passes through exactly one
*successful* CH insert in its lifetime: either the primary flush (counted there)
or, if that failed, a later DLQ drain (counted there). Failed primary flushes go
to the DLQ and are **not** counted; failed drain attempts increment the retry
counter and are **not** counted. So calling at both successful-insert sites
counts each event once — never zero, never twice.

The call happens on the **flusher / drain goroutine**, off the HTTP hot path,
after the CH commit. The core calls it synchronously; per the contract the impl
must return quickly (e.g. push to its own buffered channel) and must not panic.

## TDD steps (red → green)

Tests live in `internal/ingest/` and reuse the existing ClickHouse-backed
harness (see `flusher`/`dlq`-adjacent tests and `internal/testhelpers`). The
sink is faked: a `spySink` that appends every `(projectID, events)` call (mutex-
guarded, since the flusher runs on its own goroutine).

| # | Test (red) | Asserts |
|---|---|---|
| 1 | Default `Options` (nil `UsageSink`) → submit a batch, let it flush | Flush succeeds, events queryable; no panic. Proves the `Discard` default. |
| 2 | `spySink` → submit N events for project P, force a flush | Exactly one call: `(P, N)`. |
| 3 | `spySink` → submit a batch mixing projects P (×2) and Q (×3), flush | Two calls: `(P,2)` and `(Q,3)`; counts correct per project. |
| 4 | `spySink` + a ClickHouse that fails the first insert (forces DLQ) → flush | **No** sink call on the failed flush (events went to the DLQ, not durably accepted). |
| 5 | Continue #4: repair ClickHouse, run a DLQ drain pass | Sink called once now: `(P, N)` for the drained events. Combined with #4, the events are counted **exactly once**, at the drain. |
| 6 | `spySink` + a drain attempt that fails (CH still down), then a later successful drain | The failed attempt records nothing; only the successful drain records `(P, N)`. No double count. |

Then implement `extension.UsageSink`/`Discard`, the `Options` field + default in
`NewService`, and the two call sites until green. Keep existing ingest/flusher/DLQ
tests green — the default path is unchanged.

## Edge cases / contract notes

- **Off the hot path, but on the flusher goroutine.** A slow sink stalls the
  flusher. The contract requires the impl to be fast (buffer internally); the
  core does not spawn a goroutine per call (that would unbound concurrency under
  load). The hosted impl owns its own async aggregation.
- **Grouping.** Use a small `map[string]int` over the flushed `items`; emit in a
  stable manner. Empty batches never reach `attemptFlush` (guarded in
  `runFlusher`'s `flush`).
- **Quarantine.** A `failed_events` row that exhausts retries and is
  `QuarantineFailedEvent`-d never landed in ClickHouse, so it is **never**
  counted — correct (the customer's data was dropped; we don't bill for it).
- **Must not panic.** A buggy sink must not take down the flusher. The contract
  forbids panics; if we want belt-and-suspenders the call may be wrapped in the
  flusher's existing recovery posture — decide during implementation, but do not
  add a `recover` solely for this unless a test motivates it (YAGNI).

## Definition of done

- `extension.UsageSink` + `Discard` exist, exported, doc comments match ADR-0002.
- `ingest.Options.UsageSink` wired; nil → `Discard`; called at both
  successful-insert sites with correct per-project counts.
- Tests 1–6 green; full suite green; default behavior unchanged.
- `docs/architecture.md` "Ingest pipeline" gains one line for the seam (as-shipped).
