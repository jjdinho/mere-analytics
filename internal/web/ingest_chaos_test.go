package web_test

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"

	"github.com/jjdinho/mere-analytics/internal/ingest"
)

// These two tests exercise the *real* recovery path: they Stop and Start the
// actual Postgres/ClickHouse containers and assert the pipeline heals itself.
// The existing approximations (TestIngest_DLQOnCHFailure closes the CH handle;
// TestHealthz_ReportsDownOnFatal sets the flag directly) are irreversible, so
// they can only cover the failure half — never the drain-back/clear-on-recovery
// half these close.
//
// Why a 3s FlushInterval (vs the 50ms default)? The flusher and the DLQ drain
// both tick at FlushInterval, and a row that fails its CH replay 20 times gets
// quarantined forever (quarantineAttemptLimit). A container Stop/Start cycle
// takes several seconds, so a 50ms drain would burn the 20-attempt budget and
// quarantine the row long before the dependency returns — there'd be nothing
// left to recover. 3s keeps cumulative attempts well under 20 across a normal
// restart while still flushing promptly.

// stopContainer stops c with a short graceful window. Stop/Start (never
// Terminate) preserves the published host port, so the existing pgxpool / CH
// *sql.DB reconnect lazily on the next query.
func stopContainer(t *testing.T, c testcontainers.Container) {
	t.Helper()
	timeout := 10 * time.Second
	if err := c.Stop(context.Background(), &timeout); err != nil {
		t.Fatalf("stop container: %v", err)
	}
}

// startContainer restarts a previously stopped container.
func startContainer(t *testing.T, c testcontainers.Container) {
	t.Helper()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start container: %v", err)
	}
}

// pollUntil calls cond every 100ms until it returns true or deadline passes.
func pollUntil(deadline time.Duration, cond func() bool) bool {
	end := time.Now().Add(deadline)
	for {
		if cond() {
			return true
		}
		if !time.Now().Before(end) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// activeDLQCount returns the count of non-quarantined failed_events rows. The
// bool is false on a query error (PG mid-reconnect after a restart), so callers
// inside pollUntil simply retry.
func activeDLQCount(pool *pgxpool.Pool) (int64, bool) {
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM failed_events WHERE quarantined_at IS NULL`).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// chProjectCount returns events_raw_v1 rows for projectID. The bool is false on
// a query error (CH mid-reconnect after a restart) so pollUntil retries.
func chProjectCount(ch *sql.DB, projectID string) (int, bool) {
	var n int
	if err := ch.QueryRow(`SELECT count() FROM events_raw_v1 WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// healthzIsDown reports whether GET /healthz returns 503 with status:"down".
func healthzIsDown(t *testing.T, baseURL string) bool {
	t.Helper()
	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		return false
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode == http.StatusServiceUnavailable &&
		strings.Contains(string(body), `"status":"down"`)
}

// TestIngest_RecoverDLQDrainAfterCHRestart stops the real ClickHouse container,
// posts a batch (still 202 — the handler enqueues regardless of CH health),
// and confirms the flush fails CH but lands a failed_events row (PG up →
// writeDLQ succeeds; fatal stays clear). Then it restarts CH and asserts the
// drain *replays* the row into the recovered CH and deletes it:
// count(events_raw_v1) == submitted AND active DLQ count == 0. This is the
// recovery half TestIngest_DLQOnCHFailure (which closes the handle
// irreversibly) can never reach.
func TestIngest_RecoverDLQDrainAfterCHRestart(t *testing.T) {
	st := startIngestChaosStack(t, func(o *ingest.Options) {
		o.FlushInterval = 3 * time.Second
	})
	token, projectID := mintProjectAndToken(t, st)

	// CH down: the next flush fails the insert and must fall back to the DLQ.
	stopContainer(t, st.chContainer)

	const submitted = 3
	now := time.Now().UTC().Format(time.RFC3339Nano)
	events := make([]map[string]any, submitted)
	for i := range events {
		events[i] = map[string]any{"event": "page", "timestamp": now}
	}
	resp := postIngestBatch(t, st.srv.URL, token, map[string]any{"events": events})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST while CH down: %d want 202", resp.StatusCode)
	}

	// Failure half: a DLQ row materializes and fatal stays clear (only CH failed).
	if !pollUntil(10*time.Second, func() bool {
		n, ok := activeDLQCount(st.pgPool)
		return ok && n >= 1
	}) {
		t.Fatal("no failed_events row appeared while CH was down")
	}
	if st.ingestSvc.Flags().IsFatal() {
		t.Fatal("fatal set on CH-only failure; want clear (PG still up)")
	}

	// CH back: the drain replays the DLQ row into the recovered CH and deletes
	// it — the recovery half the close-the-handle test can't reach.
	startContainer(t, st.chContainer)

	if !pollUntil(30*time.Second, func() bool {
		dlq, dok := activeDLQCount(st.pgPool)
		ch, cok := chProjectCount(st.chAdmin, projectID)
		return dok && cok && dlq == 0 && ch == submitted
	}) {
		dlq, _ := activeDLQCount(st.pgPool)
		ch, _ := chProjectCount(st.chAdmin, projectID)
		t.Fatalf("recovery incomplete: active DLQ=%d want 0, CH count=%d want %d", dlq, ch, submitted)
	}
}

// TestIngest_RecoverFatalClearsAfterBothRestart drives the both-down cascade
// with real container stops: CH-only → batch A in the DLQ (fatal clear), then
// PG also down → batch B's flush fails CH *and* writeDLQ → SetFatal(true), B
// dropped. After PG returns the client sees 503 (fatal), not 500; after CH
// returns the drain replays batch A's pre-existing DLQ row, which clears fatal.
// Batch B stays lost — asserted as such.
//
// Two deviations from the original handoff, both forced by the architecture and
// surfaced rather than papered over:
//
//   - Batch B is enqueued via ingestSvc.Submit, not an HTTP POST. The ingest
//     token middleware (requirePublicToken → LookupIngestToken) queries PG, so
//     a POST with PG down 500s at auth before reaching the pipeline — it can
//     never drive a both-down flush. Submit only enqueues (no PG), so the
//     flusher's writeDLQ is what hits the dead PG.
//   - The "client POST → 503" assertion runs after PG is restarted (PG up, CH
//     still down, fatal still set), not while both are down, for the same
//     reason: a POST during a full PG outage 500s at auth, so the postIngest
//     fatal-503 branch is only client-observable once PG is back.
func TestIngest_RecoverFatalClearsAfterBothRestart(t *testing.T) {
	st := startIngestChaosStack(t, func(o *ingest.Options) {
		o.FlushInterval = 3 * time.Second
	})
	token, projectID := mintProjectAndToken(t, st)

	// Phase 1 — CH down, PG up: batch A lands in the DLQ; fatal stays clear.
	stopContainer(t, st.chContainer)
	const aCount = 3
	now := time.Now().UTC().Format(time.RFC3339Nano)
	eventsA := make([]map[string]any, aCount)
	for i := range eventsA {
		eventsA[i] = map[string]any{"event": "alpha", "timestamp": now}
	}
	respA := postIngestBatch(t, st.srv.URL, token, map[string]any{"events": eventsA})
	respA.Body.Close()
	if respA.StatusCode != http.StatusAccepted {
		t.Fatalf("POST A: %d want 202", respA.StatusCode)
	}
	if !pollUntil(10*time.Second, func() bool {
		n, ok := activeDLQCount(st.pgPool)
		return ok && n >= 1
	}) {
		t.Fatal("batch A never produced a DLQ row")
	}
	if st.ingestSvc.Flags().IsFatal() {
		t.Fatal("fatal set after CH-only failure; want clear")
	}

	// Phase 2 — both down: batch B's flush fails CH *and* writeDLQ (PG down) →
	// SetFatal(true), B dropped. Enqueued via Submit (see doc comment).
	stopContainer(t, st.pgContainer)
	const bCount = 2
	bt := time.Now().UTC()
	eventsB := make([]ingest.Event, bCount)
	for i := range eventsB {
		eventsB[i] = ingest.Event{Event: "bravo", Timestamp: bt}
	}
	if err := st.ingestSvc.Submit(context.Background(), projectID, eventsB); err != nil {
		t.Fatalf("submit B: %v", err)
	}
	if !pollUntil(15*time.Second, func() bool {
		return st.ingestSvc.Flags().IsFatal()
	}) {
		t.Fatal("fatal never set after the both-down flush")
	}

	// /healthz reports down even with both deps gone — it reads flags only.
	if !healthzIsDown(t, st.srv.URL) {
		t.Fatal("/healthz not 503/down while fatal set")
	}

	// Phase 3 — PG back, CH still down: fatal persists; a client POST now 503s
	// (fatal), no longer 500 (auth). Poll because pgxpool reconnects lazily.
	startContainer(t, st.pgContainer)
	if !pollUntil(20*time.Second, func() bool {
		resp := postIngestBatch(t, st.srv.URL, token, map[string]any{
			"events": []map[string]any{{"event": "gamma", "timestamp": now}},
		})
		resp.Body.Close()
		return resp.StatusCode == http.StatusServiceUnavailable
	}) {
		t.Fatal("fresh POST never returned 503 after PG restart (fatal still set)")
	}
	if !st.ingestSvc.Flags().IsFatal() {
		t.Fatal("fatal cleared while CH still down")
	}

	// Phase 4 — CH back: the drain replays batch A's DLQ row, deletes it, and
	// clears fatal. Batch B stays lost (its writeDLQ failed → no row to replay).
	startContainer(t, st.chContainer)
	if !pollUntil(30*time.Second, func() bool {
		dlq, dok := activeDLQCount(st.pgPool)
		return dok && dlq == 0 && !st.ingestSvc.Flags().IsFatal()
	}) {
		dlq, _ := activeDLQCount(st.pgPool)
		t.Fatalf("recovery incomplete: fatal=%v active DLQ=%d want false/0",
			st.ingestSvc.Flags().IsFatal(), dlq)
	}

	// Only batch A's events were replayed; batch B is gone for good.
	if ch, ok := chProjectCount(st.chAdmin, projectID); !ok || ch != aCount {
		t.Fatalf("CH count=%d (ok=%v) want %d (batch A only; B lost)", ch, ok, aCount)
	}
}
