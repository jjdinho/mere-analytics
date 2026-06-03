package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jjdinho/mere-analytics/extension"
	"github.com/jjdinho/mere-analytics/internal/oauth"
)

// stubLimiter is a programmable RateLimiter that records every LimitKey it is
// asked about. Mutex-guarded because Allow can be called from many goroutines.
type stubLimiter struct {
	mu         sync.Mutex
	ok         bool
	retryAfter time.Duration
	keys       []extension.LimitKey
}

func (s *stubLimiter) Allow(_ context.Context, key extension.LimitKey) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, key)
	return s.ok, s.retryAfter
}

func (s *stubLimiter) calls() []extension.LimitKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]extension.LimitKey, len(s.keys))
	copy(out, s.keys)
	return out
}

// spyHandler records whether it ran so deny tests can prove the wrapped handler
// was short-circuited.
type spyHandler struct{ called bool }

func (h *spyHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.called = true
	w.WriteHeader(http.StatusAccepted)
}

// ingestReq builds an ingest-surface request with the project already resolved
// onto the context, exactly as requirePublicToken leaves it for the limiter.
func ingestReq(projectID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/events", nil)
	return r.WithContext(context.WithValue(r.Context(), projectCtxKey{}, projectID))
}

// bearerReq builds a query/MCP-surface request with the OAuth grant already on
// the context, exactly as RequireBearer leaves it for the limiter.
func bearerReq(userID, projectID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/query", nil)
	return r.WithContext(oauth.ContextWith(r.Context(), &oauth.AccessContext{
		UserID:    userID,
		ProjectID: projectID,
	}))
}

func TestRateLimit_DenyWritesRetryAfterAndSkipsHandler(t *testing.T) {
	lim := &stubLimiter{ok: false, retryAfter: 2 * time.Second}
	next := &spyHandler{}
	rec := httptest.NewRecorder()

	rateLimit(lim, "ingest")(next).ServeHTTP(rec, ingestReq("proj-1"))

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status: %d want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "2" {
		t.Errorf("Retry-After: %q want 2", ra)
	}
	if next.called {
		t.Error("wrapped handler ran despite deny")
	}
}

func TestRateLimit_AllowPassesIngestKey(t *testing.T) {
	lim := &stubLimiter{ok: true}
	next := &spyHandler{}
	rec := httptest.NewRecorder()

	rateLimit(lim, "ingest")(next).ServeHTTP(rec, ingestReq("proj-1"))

	if !next.called {
		t.Fatal("wrapped handler did not run on allow")
	}
	if rec.Code != http.StatusAccepted {
		t.Errorf("status: %d want 202", rec.Code)
	}
	keys := lim.calls()
	if len(keys) != 1 {
		t.Fatalf("limiter consulted %d times, want exactly 1", len(keys))
	}
	k := keys[0]
	if k.Surface != "ingest" {
		t.Errorf("Surface: %q want ingest", k.Surface)
	}
	if k.ProjectID != "proj-1" {
		t.Errorf("ProjectID: %q want proj-1", k.ProjectID)
	}
	if k.UserID != "" {
		t.Errorf("UserID: %q want empty (ingest has no user)", k.UserID)
	}
	if k.RemoteIP == "" {
		t.Error("RemoteIP empty, want the request's remote address")
	}
}

func TestRateLimit_DenyZeroHintOmitsRetryAfter(t *testing.T) {
	lim := &stubLimiter{ok: false, retryAfter: 0}
	rec := httptest.NewRecorder()

	rateLimit(lim, "ingest")(&spyHandler{}).ServeHTTP(rec, ingestReq("proj-1"))

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status: %d want 429", rec.Code)
	}
	if _, ok := rec.Header()["Retry-After"]; ok {
		t.Errorf("Retry-After present, want omitted for a zero hint")
	}
}

func TestRateLimit_AllowPassesBearerKey(t *testing.T) {
	lim := &stubLimiter{ok: true}
	next := &spyHandler{}
	rec := httptest.NewRecorder()

	rateLimit(lim, "query")(next).ServeHTTP(rec, bearerReq("user-7", "proj-9"))

	if !next.called {
		t.Fatal("wrapped handler did not run on allow")
	}
	keys := lim.calls()
	if len(keys) != 1 {
		t.Fatalf("limiter consulted %d times, want exactly 1", len(keys))
	}
	k := keys[0]
	if k.Surface != "query" {
		t.Errorf("Surface: %q want query", k.Surface)
	}
	if k.ProjectID != "proj-9" || k.UserID != "user-7" {
		t.Errorf("identity: ProjectID=%q UserID=%q want proj-9/user-7", k.ProjectID, k.UserID)
	}
	if k.RemoteIP == "" {
		t.Error("RemoteIP empty, want the request's remote address")
	}
}
