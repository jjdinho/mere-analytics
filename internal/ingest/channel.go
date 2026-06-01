package ingest

import (
	"errors"
	"sync/atomic"
)

// ErrChannelFull is returned by Submit when the in-flight event count would
// exceed the configured ceiling. Surfaced to clients as 503 + Retry-After: 1.
var ErrChannelFull = errors.New("ingest: channel full")

// channel pairs a buffered chan of envelopes with an atomic counter of
// pending events (NOT envelopes). Submit gates on the atomic *first* and
// only enqueues the envelope after a successful CAS, so a single oversized
// batch can't blow past the ceiling and so concurrent submitters never
// race past each other (Issue 8A).
//
// The chan capacity is intentionally much smaller than the event ceiling:
// envelopes carry up to FlushEvents at once, so the chan only needs enough
// slots for envelope-level backpressure. The pending counter is the
// authoritative gate.
type channel struct {
	ch       chan batchEnvelope
	pending  atomic.Int64
	capacity int64
}

func newChannel(eventCapacity int, chanCapacity int) *channel {
	return &channel{
		ch:       make(chan batchEnvelope, chanCapacity),
		capacity: int64(eventCapacity),
	}
}

// submit reserves len(env.Events) slots on the pending counter (CAS loop, no
// mutex) and then enqueues. If the chan is unexpectedly full (shouldn't
// happen with correct sizing, but possible during shutdown), the reservation
// is rolled back. Returns ErrChannelFull on either gate failure.
func (c *channel) submit(env batchEnvelope) error {
	n := int64(len(env.Events))
	if n == 0 {
		return nil
	}
	for {
		cur := c.pending.Load()
		if cur+n > c.capacity {
			return ErrChannelFull
		}
		if c.pending.CompareAndSwap(cur, cur+n) {
			break
		}
	}
	select {
	case c.ch <- env:
		return nil
	default:
		c.pending.Add(-n)
		return ErrChannelFull
	}
}

// drained is called by the flusher after a batch is consumed (either flushed
// to CH or written to the DLQ). Decrement is unconditional — both terminal
// states free the slots.
func (c *channel) drained(n int) {
	c.pending.Add(-int64(n))
}

// recv blocks until an envelope is available or the chan closes. ok=false
// signals close.
func (c *channel) recv() (batchEnvelope, bool) {
	env, ok := <-c.ch
	return env, ok
}

// pendingCount is exposed for tests + the flusher's shutdown loop.
func (c *channel) pendingCount() int64 { return c.pending.Load() }

// close shuts the input gate. Safe to call from a single Shutdown caller.
func (c *channel) close() { close(c.ch) }
