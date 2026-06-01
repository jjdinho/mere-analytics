package web

import (
	"encoding/json"
	"net/http"

	"github.com/jjdinho/mere-analytics/internal/oauth"
)

// getWhoami is the bearer-auth smoke surface. It exists so RequireBearer is
// exercised end-to-end in the real production handler chain (not just in a
// test stub) and so self-hosters have a debug endpoint to verify a token.
// /v1/query + /mcp will mount with the same pattern when those handlers land.
func getWhoami() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac := oauth.FromContext(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_id":    ac.UserID,
			"project_id": ac.ProjectID,
			"client_id":  ac.ClientID,
			"scope":      ac.Scope,
		})
	}
}
