package ingest_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"

	"github.com/jjdinho/mere-analytics/extension"
	"github.com/jjdinho/mere-analytics/internal/clickhouse"
	"github.com/jjdinho/mere-analytics/internal/config"
	"github.com/jjdinho/mere-analytics/internal/idgen"
	"github.com/jjdinho/mere-analytics/internal/ingest"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

// usageCall captures one RecordIngested invocation.
type usageCall struct {
	projectID string
	events    int
}

// spySink is a fake extension.UsageSink that appends every call. Mutex-guarded
// because RecordIngested runs on the flusher / drain goroutine, not the test
// goroutine.
type spySink struct {
	mu    sync.Mutex
	calls []usageCall
}

func (s *spySink) RecordIngested(_ context.Context, projectID string, events int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, usageCall{projectID, events})
}

func (s *spySink) snapshot() []usageCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]usageCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// totals folds the recorded calls into a per-project event sum, independent of
// how the calls were grouped.
func (s *spySink) totals() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]int{}
	for _, c := range s.calls {
		m[c.projectID] += c.events
	}
	return m
}

// newIngestService brings up ephemeral PG + CH (no restart), migrates both, and
// returns a started Service wired to sink. Used by the happy-path tests.
func newIngestService(t *testing.T, sink extension.UsageSink, opts ...func(*ingest.Options)) (*ingest.Service, *pgxpool.Pool, *sql.DB) {
	t.Helper()
	pgPool, pgCfg := testhelpers.StartPostgres(t)
	chAdmin, chCfg := testhelpers.StartClickHouse(t)
	svc := assembleService(t, pgPool, pgCfg, chAdmin, chCfg, sink, opts...)
	return svc, pgPool, chAdmin
}

// newIngestServiceChaos is newIngestService with a pinned-port CH container so
// the test can Stop/Start it to drive the DLQ path. PG is ephemeral (never
// restarted).
func newIngestServiceChaos(t *testing.T, sink extension.UsageSink, opts ...func(*ingest.Options)) (*ingest.Service, *pgxpool.Pool, *sql.DB, testcontainers.Container) {
	t.Helper()
	pgPool, pgCfg := testhelpers.StartPostgres(t)
	chAdmin, chCfg, chCont := testhelpers.StartClickHouseC(t)
	svc := assembleService(t, pgPool, pgCfg, chAdmin, chCfg, sink, opts...)
	return svc, pgPool, chAdmin, chCont
}

func assembleService(
	t *testing.T,
	pgPool *pgxpool.Pool, pgCfg config.Config,
	chAdmin *sql.DB, chCfg config.Config,
	sink extension.UsageSink,
	opts ...func(*ingest.Options),
) *ingest.Service {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	pgDrv, err := postgres.MigrateDriver(pgCfg)
	if err != nil {
		t.Fatalf("pg migrate driver: %v", err)
	}
	if err := mmigrate.Run(ctx, "pg", pgDrv, migrations.Postgres, "postgres", logger); err != nil {
		t.Fatalf("pg migrate: %v", err)
	}
	chDrv, err := clickhouse.MigrateDriver(chAdmin, chCfg)
	if err != nil {
		t.Fatalf("ch migrate driver: %v", err)
	}
	if err := mmigrate.Run(ctx, "ch", chDrv, migrations.ClickHouse, "clickhouse", logger); err != nil {
		t.Fatalf("ch migrate: %v", err)
	}

	options := ingest.Options{
		EventBuffer:          50_000,
		FlushEvents:          5_000,
		FlushInterval:        time.Hour, // happy-path tests flush via Shutdown; chaos tests override
		ShutdownGrace:        10 * time.Second,
		MaxBodyBytes:         10 * 1024 * 1024,
		DLQDrainBatchLimit:   10,
		DLQDepth503Threshold: 100_000,
		UsageSink:            sink,
	}
	for _, opt := range opts {
		opt(&options)
	}
	svc := ingest.NewService(pgPool, chAdmin, options, logger)
	svc.Start(ctx)
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = svc.Shutdown(shutCtx)
	})
	return svc
}

func makeEvents(name string, n int) []ingest.Event {
	now := time.Now().UTC()
	out := make([]ingest.Event, n)
	for i := range out {
		out[i] = ingest.Event{Event: name, Timestamp: now}
	}
	return out
}

// flushViaShutdown forces a single deterministic flush of everything buffered:
// Shutdown closes the input gate, the flusher drains the buffer in one
// attemptFlush, and (CH up) records usage synchronously before returning.
func flushViaShutdown(t *testing.T, svc *ingest.Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := svc.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown flush: %v", err)
	}
}

func chCount(t *testing.T, ch *sql.DB, projectID string) int {
	t.Helper()
	var n int
	if err := ch.QueryRow(`SELECT count() FROM events_raw_v1 WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		t.Fatalf("ch count: %v", err)
	}
	return n
}

func activeDLQ(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM failed_events WHERE quarantined_at IS NULL`).Scan(&n); err != nil {
		return -1
	}
	return n
}

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

func stopContainer(t *testing.T, c testcontainers.Container) {
	t.Helper()
	timeout := 10 * time.Second
	if err := c.Stop(context.Background(), &timeout); err != nil {
		t.Fatalf("stop container: %v", err)
	}
}

func startContainer(t *testing.T, c testcontainers.Container) {
	t.Helper()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start container: %v", err)
	}
}

// Test #1 — nil UsageSink defaults to Discard: the batch still flushes and lands
// in ClickHouse with no panic. Proves the open-source default is unchanged.
func TestUsageSink_DefaultDiscard(t *testing.T) {
	svc, _, chAdmin := newIngestService(t, nil)
	pid := idgen.New()
	if err := svc.Submit(context.Background(), pid, makeEvents("pageview", 4)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	flushViaShutdown(t, svc)
	if got := chCount(t, chAdmin, pid); got != 4 {
		t.Errorf("ch count: %d want 4", got)
	}
}

// Test #2 — single project: exactly one RecordIngested call (P, N).
func TestUsageSink_SingleProjectCountedOnce(t *testing.T) {
	sink := &spySink{}
	svc, _, _ := newIngestService(t, sink)
	p := idgen.New()
	if err := svc.Submit(context.Background(), p, makeEvents("pageview", 7)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	flushViaShutdown(t, svc)

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls: %d want 1 (%v)", len(calls), calls)
	}
	if calls[0] != (usageCall{p, 7}) {
		t.Errorf("call: %v want {%s 7}", calls[0], p)
	}
}

// Test #3 — a batch mixing projects emits one call per distinct project with the
// correct per-project counts.
func TestUsageSink_MixedProjectsCountedPerProject(t *testing.T) {
	sink := &spySink{}
	svc, _, _ := newIngestService(t, sink)
	p, q := idgen.New(), idgen.New()
	if err := svc.Submit(context.Background(), p, makeEvents("a", 2)); err != nil {
		t.Fatalf("submit P: %v", err)
	}
	if err := svc.Submit(context.Background(), q, makeEvents("b", 3)); err != nil {
		t.Fatalf("submit Q: %v", err)
	}
	flushViaShutdown(t, svc)

	calls := sink.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls: %d want 2 (%v)", len(calls), calls)
	}
	if got := sink.totals(); got[p] != 2 || got[q] != 3 || len(got) != 2 {
		t.Errorf("totals: %v want {%s:2 %s:3}", got, p, q)
	}
}

// Test #4+#5+#6 — exactly once at the DLQ drain. With CH down, the primary flush
// fails and the batch lands in the DLQ; the sink records NOTHING (events not
// durably accepted), and repeated failed drain attempts still record nothing.
// When CH recovers, the drain replays the row and records the events exactly
// once — never zero, never twice.
func TestUsageSink_CountedExactlyOnceAtDrain(t *testing.T) {
	sink := &spySink{}
	svc, pgPool, chAdmin, chCont := newIngestServiceChaos(t, sink, func(o *ingest.Options) {
		o.FlushInterval = time.Second // brisk flush+drain, well under the 20-attempt quarantine budget
	})

	// CH down → the next flush fails its insert and must fall back to the DLQ.
	stopContainer(t, chCont)
	const n = 3
	p := idgen.New()
	if err := svc.Submit(context.Background(), p, makeEvents("page", n)); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// A DLQ row materializes (PG up → writeDLQ succeeds) and the sink stays empty.
	if !pollUntil(15*time.Second, func() bool { return activeDLQ(t, pgPool) >= 1 }) {
		t.Fatal("no failed_events row appeared while CH was down")
	}
	// Let a couple more drain ticks run; failed drain attempts must record nothing.
	time.Sleep(2500 * time.Millisecond)
	if calls := sink.snapshot(); len(calls) != 0 {
		t.Fatalf("sink recorded %v while CH down; want nothing (events not durably accepted)", calls)
	}

	// CH back → the drain replays the DLQ row and records the events exactly once.
	startContainer(t, chCont)
	if !pollUntil(30*time.Second, func() bool {
		return activeDLQ(t, pgPool) == 0 && chCount(t, chAdmin, p) == n
	}) {
		t.Fatalf("recovery incomplete: DLQ=%d CH=%d want 0/%d", activeDLQ(t, pgPool), chCount(t, chAdmin, p), n)
	}

	calls := sink.snapshot()
	if len(calls) != 1 || calls[0] != (usageCall{p, n}) {
		t.Fatalf("calls: %v want exactly one {%s %d} (counted once at drain, never twice)", calls, p, n)
	}
}
