package web

import (
	"net/http"

	"github.com/jjdinho/mere-analytics/extension"
	"github.com/jjdinho/mere-analytics/internal/oauth"
)

// overQuotaMessage is the default denial text when the Entitlement seam returns
// no reason of its own. Deliberately generic — the wrapper supplies a specific
// hint (and a billing link via the upgrade page) when it has one.
const overQuotaMessage = "analysis is over the plan limit for this project; upgrade to continue"

// entitle is the extension.Entitlement call site for the bearer-authenticated
// analysis surfaces (/api/v1/.../query + .../schema). Like rateLimit, it must
// be mounted *after* RequireBearer so the grant's ProjectID is on the context;
// it gates on that project (which is always the caller's own — the token is
// bound to it), so a 402 here leaks nothing a 200 wouldn't.
//
// On deny it writes 402 Payment Required and the wrapped handler never runs —
// distinct from rateLimit's 429, because over-quota is "pay to continue", not
// "retry later". The open-source build wires extension.Unlimited, so this is a
// pass-through unless a wrapper injects a real Entitlement.
func entitle(ent extension.Entitlement) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var projectID string
			if ac := oauth.FromContext(r.Context()); ac != nil {
				projectID = ac.ProjectID
			}
			if ok, reason := ent.AllowAnalysis(r.Context(), projectID); !ok {
				if reason == "" {
					reason = overQuotaMessage
				}
				http.Error(w, reason, http.StatusPaymentRequired)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// gateAnalysisHTML is the Entitlement call site for the session-authenticated
// web query playground. It is called by the handler *after* the project's
// visibility has been confirmed, so an over-quota deny can never reveal a
// project the viewer can't already see (404-before-402, matching the rest of
// the tenant-isolation surface).
//
// It returns true when it has written the response (the caller must stop): an
// over-quota project redirects to the wrapper's upgrade page (upgradeURL), or
// — if none is configured — falls back to a plain 402 so the lock still holds.
// Returns false (no write) when analysis is allowed and the handler should run.
func gateAnalysisHTML(w http.ResponseWriter, r *http.Request, ent extension.Entitlement, upgradeURL, projectID string) bool {
	if ok, _ := ent.AllowAnalysis(r.Context(), projectID); ok {
		return false
	}
	if upgradeURL == "" {
		http.Error(w, overQuotaMessage, http.StatusPaymentRequired)
		return true
	}
	http.Redirect(w, r, upgradeURL, http.StatusSeeOther)
	return true
}
