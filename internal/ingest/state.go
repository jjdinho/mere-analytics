package ingest

import "sync/atomic"

// Flags exposes the process-level ingest state: a fatal flag set when both CH
// and the PG dead-letter queue fail in the same flush, a disabled flag for
// the INGEST_DISABLED kill switch + SIGTERM phase 1, and a DLQ depth gauge
// refreshed once per drain pass.
//
// All three are atomic; readers are on the hot path (every /v1/ingest
// request + /healthz poll).
type Flags struct {
	fatal    atomic.Bool
	disabled atomic.Bool
	dlqDepth atomic.Int64
}

// IsFatal reports whether the flusher has hit a CH+PG dual failure since the
// last clean flush. While true, /v1/ingest returns 503 + /healthz reports
// down. Cleared by the first clean flush after CH recovers.
func (f *Flags) IsFatal() bool { return f.fatal.Load() }

// SetFatal records or clears the fatal state.
func (f *Flags) SetFatal(v bool) { f.fatal.Store(v) }

// IsDisabled reports whether the kill switch is active. Triggered at boot
// by INGEST_DISABLED=true and by SIGTERM phase 1 immediately before HTTP
// shutdown.
func (f *Flags) IsDisabled() bool { return f.disabled.Load() }

// SetDisabled flips the kill switch.
func (f *Flags) SetDisabled(v bool) { f.disabled.Store(v) }

// DLQDepth returns the count refreshed by the drain goroutine at the end of
// each pass. /healthz compares it against DLQDepth503Threshold.
func (f *Flags) DLQDepth() int64 { return f.dlqDepth.Load() }

// SetDLQDepth is called by the drain goroutine after a CountActiveFailedEvents.
func (f *Flags) SetDLQDepth(v int64) { f.dlqDepth.Store(v) }
