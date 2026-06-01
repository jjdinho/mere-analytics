// Package ingest implements the analytics write path: POST /v1/ingest →
// validated batch → in-memory channel → flusher → ClickHouse events_raw_v1.
//
// Pipeline:
//
//	    handler ──► Submit (atomic pending++, chan <- env)
//	                          │
//	                          ▼
//	                  ┌───── chan batchEnvelope ─────┐
//	                  │                              │
//	                  ▼                              │
//	            flusher goroutine                    │
//	            (accumulate to FlushEvents or                    │
//	             tick at FlushInterval)                          │
//	                  │
//	            ┌─────┴──────┐
//	            │            │
//	            ▼            ▼
//	   CH INSERT ok    CH INSERT fails
//	            │            │
//	            │       InsertFailedEvent (DLQ row, batch as JSONB)
//	            │            │
//	            │       ┌────┴─────┐
//	            │       ▼          ▼
//	            │   PG ok      PG also fails
//	            │   │             │
//	            │   continue   SetFatal(true)
//	            ▼
//	          drain.atomic.pending -= len(batch)
//
//	DLQ drain goroutine (every FlushInterval):
//	    ListFailedEventsForDrain LIMIT N
//	          │
//	          ├── CH ok                 ──► DeleteFailedEvent
//	          └── CH fails              ──► IncrementFailedEventAttempt
//	                  attempt+1 ≥ 20    ──► QuarantineFailedEvent
//	                  age ≥ 24h         ──► QuarantineFailedEvent
//	    After pass: Flags.SetDLQDepth(CountActiveFailedEvents)
//
// Token paths stay strictly separate. /v1/ingest uses LookupIngestToken
// against api_tokens (mere_pub_…); /v1/whoami + future /v1/query + /mcp use
// oauth.Service.LookupActiveAccessToken against oauth_access_tokens. The
// two surfaces share no code, and the OAuth path rejects mere_pub_ at the
// prefix step.
package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/postgres/db"
)

// Event is the wire-stable representation of a single analytics event.
//
// DO NOT rename fields. failed_events.batch_payload stores Event JSON
// captured at submission time, so a rename here would orphan in-flight DLQ
// rows on the next deploy.
type Event struct {
	Event      string          `json:"event"`
	DistinctID *string         `json:"distinct_id,omitempty"`
	Timestamp  time.Time       `json:"timestamp"`
	SessionID  *string         `json:"session_id,omitempty"`
	Properties json.RawMessage `json:"properties,omitempty"`
	Extras     json.RawMessage `json:"extras,omitempty"`
}

// batchEnvelope is the unit of work carried over the chan: a validated event
// slice tagged with the resolving project.
type batchEnvelope struct {
	ProjectID string
	Events    []Event
}

// Options configures a Service. Built from config.Config in cmd/server.
//
// EventBuffer is the atomic pending-event ceiling; FlushEvents is the
// per-batch flush trigger; FlushInterval is the time-based fallback;
// ShutdownGrace bounds SIGTERM phase 3; Disabled seeds the kill switch;
// MaxBodyBytes is informational (the web layer enforces); DLQDrainBatchLimit
// caps each drain pass; DLQDepth503Threshold is informational (web layer
// enforces).
type Options struct {
	EventBuffer          int
	FlushEvents          int
	FlushInterval        time.Duration
	ShutdownGrace        time.Duration
	Disabled             bool
	MaxBodyBytes         int64
	DLQDrainBatchLimit   int
	DLQDepth503Threshold int
}

// Service owns the ingest pipeline's runtime state: the in-memory channel,
// the flusher + DLQ goroutines, the flags surface, and the handles to the
// two backing stores.
type Service struct {
	pool   *pgxpool.Pool
	q      *db.Queries
	ch     *sql.DB
	opts   Options
	logger *slog.Logger
	flags  *Flags
	chBuf  *channel

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	flusherWG sync.WaitGroup
}

// NewService constructs a Service. Call Start to launch the flusher + DLQ
// goroutines and Shutdown on SIGTERM phase 3 to drain.
func NewService(pool *pgxpool.Pool, ch *sql.DB, opts Options, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	chanCap := opts.EventBuffer / opts.FlushEvents
	if chanCap < 100 {
		chanCap = 100
	}
	s := &Service{
		pool:   pool,
		q:      db.New(pool),
		ch:     ch,
		opts:   opts,
		logger: logger,
		flags:  &Flags{},
		chBuf:  newChannel(opts.EventBuffer, chanCap),
		stopCh: make(chan struct{}),
	}
	s.flags.SetDisabled(opts.Disabled)
	return s
}

// Flags exposes the process-level ingest state surface so handlers + the
// /healthz route can read fatal / disabled / DLQ depth without poking the
// Service's internals.
func (s *Service) Flags() *Flags { return s.flags }

// Options returns the configured option bag (mostly for the web layer to pull
// MaxBodyBytes and DLQDepth503Threshold off a single dependency).
func (s *Service) OptionsView() Options { return s.opts }

// Submit hands a validated event slice to the in-memory channel. Returns
// ErrChannelFull when the in-flight event count would exceed EventBuffer;
// the handler translates that to 503 + Retry-After: 1.
//
// Submit does not block on a full channel — backpressure is fail-fast so a
// slow flusher can't hold open client connections forever.
func (s *Service) Submit(_ context.Context, projectID string, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	return s.chBuf.submit(batchEnvelope{ProjectID: projectID, Events: events})
}

// LookupIngestToken resolves a plaintext mere_pub_… bearer to its project ID.
//
// Returns ("", nil) for the three "no project" cases (missing prefix, no row,
// or soft-deleted project) — the middleware uniformly turns those into 401.
// Returns ("", err) on infrastructure failure (PG down, network blip); the
// middleware *must* propagate that as 500, never 401, so PG-outage telemetry
// doesn't look like an attack signal.
func (s *Service) LookupIngestToken(ctx context.Context, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if !strings.HasPrefix(plaintext, auth.PublicTokenPrefix) {
		return "", nil
	}
	hashHex := auth.HashToken(plaintext)
	row, err := s.q.GetActiveIngestTokenByHash(ctx, hashHex)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("lookup ingest token: %w", err)
	}
	return row.ProjectID, nil
}
