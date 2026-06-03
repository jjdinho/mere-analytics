package web_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jjdinho/mere-analytics/extension"
)

// recordingLimiter is a programmable RateLimiter for the real-chain wiring
// tests. It records each LimitKey so the test can confirm the limiter ran after
// the tenant resolved (ProjectID populated). Mutex-guarded — the handler chain
// may serve concurrently.
type recordingLimiter struct {
	mu         sync.Mutex
	ok         bool
	retryAfter time.Duration
	keys       []extension.LimitKey
}

func (l *recordingLimiter) Allow(_ context.Context, key extension.LimitKey) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
	return l.ok, l.retryAfter
}

func (l *recordingLimiter) recorded() []extension.LimitKey {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]extension.LimitKey, len(l.keys))
	copy(out, l.keys)
	return out
}

// TestIngest_RateLimitDefaultUnchanged proves the no-op default: a handler built
// with a nil RateLimiter (the open-source build) substitutes extension.AllowAll,
// so a valid batch still lands 202 — byte-for-byte the pre-seam behavior.
func TestIngest_RateLimitDefaultUnchanged(t *testing.T) {
	st := startIngestStack(t) // nil limiter → AllowAll
	token, _ := mintProjectAndToken(t, st)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	resp := postIngestBatch(t, st.srv.URL, token, map[string]any{
		"events": []map[string]any{{"event": "x", "timestamp": now}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status: %d want 202 (nil limiter must default to allow-all)", resp.StatusCode)
	}
}

// TestIngest_RateLimitDenyOnRealChain injects a deny-all limiter and posts a
// valid batch through the real ingest chain. The seam must short-circuit with
// 429 + Retry-After *after* requirePublicToken resolved the project (so the
// captured key carries the tenant), and postIngest must never run — proven by
// the events never reaching ClickHouse.
func TestIngest_RateLimitDenyOnRealChain(t *testing.T) {
	lim := &recordingLimiter{ok: false, retryAfter: 2 * time.Second}
	st := startIngestStackWithLimiter(t, lim)
	token, projectID := mintProjectAndToken(t, st)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	resp := postIngestBatch(t, st.srv.URL, token, map[string]any{
		"events": []map[string]any{{"event": "x", "timestamp": now}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status: %d want 429", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "2" {
		t.Errorf("Retry-After: %q want 2", ra)
	}

	keys := lim.recorded()
	if len(keys) != 1 {
		t.Fatalf("limiter consulted %d times, want exactly 1", len(keys))
	}
	if keys[0].Surface != "ingest" {
		t.Errorf("Surface: %q want ingest", keys[0].Surface)
	}
	if keys[0].ProjectID != projectID {
		t.Errorf("ProjectID: %q want %q (limiter must run after tenant resolution)", keys[0].ProjectID, projectID)
	}

	// Denied: the events never reached the pipeline, so nothing lands in CH.
	if got := waitForCHCount(t, st.chAdmin, projectID, 1, time.Second); got != 0 {
		t.Errorf("ch count: %d want 0 (denied request must not enqueue)", got)
	}
}
