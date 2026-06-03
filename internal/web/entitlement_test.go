package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stubEntitlement is a programmable Entitlement that records the project ids it
// was asked about. Mutex-guarded because AllowAnalysis can be called from many
// goroutines on the hot path.
type stubEntitlement struct {
	mu     sync.Mutex
	ok     bool
	reason string
	asked  []string
}

func (s *stubEntitlement) AllowAnalysis(_ context.Context, projectID string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, projectID)
	return s.ok, s.reason
}

func (s *stubEntitlement) projects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.asked))
	copy(out, s.asked)
	return out
}

func TestEntitle_DenyWrites402AndSkipsHandler(t *testing.T) {
	ent := &stubEntitlement{ok: false, reason: "over 1M events"}
	next := &spyHandler{}
	rec := httptest.NewRecorder()

	entitle(ent)(next).ServeHTTP(rec, bearerReq("user-7", "proj-9"))

	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("status: %d want 402", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "over 1M events") {
		t.Errorf("body %q does not carry the deny reason", body)
	}
	if next.called {
		t.Error("wrapped handler ran despite deny")
	}
	if got := ent.projects(); len(got) != 1 || got[0] != "proj-9" {
		t.Errorf("gated on %v, want [proj-9] (the grant's project)", got)
	}
}

func TestEntitle_DenyEmptyReasonUsesDefault(t *testing.T) {
	ent := &stubEntitlement{ok: false}
	rec := httptest.NewRecorder()

	entitle(ent)(&spyHandler{}).ServeHTTP(rec, bearerReq("user-7", "proj-9"))

	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("status: %d want 402", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "upgrade") {
		t.Errorf("body %q does not carry the default over-quota message", body)
	}
}

func TestEntitle_AllowRunsHandler(t *testing.T) {
	ent := &stubEntitlement{ok: true}
	next := &spyHandler{}
	rec := httptest.NewRecorder()

	entitle(ent)(next).ServeHTTP(rec, bearerReq("user-7", "proj-9"))

	if !next.called {
		t.Fatal("wrapped handler did not run on allow")
	}
	if rec.Code != http.StatusAccepted {
		t.Errorf("status: %d want 202", rec.Code)
	}
}

func TestGateAnalysisHTML_DenyRedirectsToUpgrade(t *testing.T) {
	ent := &stubEntitlement{ok: false}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/projects/proj-9/query", nil)

	denied := gateAnalysisHTML(rec, r, ent, "https://pay.example/billing", "proj-9")

	if !denied {
		t.Fatal("gate returned false (allowed) on a deny")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status: %d want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://pay.example/billing" {
		t.Errorf("Location: %q want the upgrade URL", loc)
	}
}

func TestGateAnalysisHTML_DenyNoUpgradeURLFallsBackTo402(t *testing.T) {
	ent := &stubEntitlement{ok: false}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/projects/proj-9/query", nil)

	denied := gateAnalysisHTML(rec, r, ent, "", "proj-9")

	if !denied {
		t.Fatal("gate returned false (allowed) on a deny")
	}
	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("status: %d want 402", rec.Code)
	}
}

func TestGateAnalysisHTML_AllowDoesNotWrite(t *testing.T) {
	ent := &stubEntitlement{ok: true}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/projects/proj-9/query", nil)

	if denied := gateAnalysisHTML(rec, r, ent, "https://pay.example/billing", "proj-9"); denied {
		t.Fatal("gate returned true (denied) on an allow")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d want 200 (untouched recorder default)", rec.Code)
	}
}
