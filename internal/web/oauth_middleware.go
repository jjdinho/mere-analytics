package web

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/jjdinho/mere-analytics/internal/oauth"
)

// RequireBearer enforces an OAuth bearer token on the wrapped handler. Missing,
// malformed, expired, or revoked tokens get a uniform 401 with a
// WWW-Authenticate header — we don't distinguish failure modes per RFC 6750 §3.
//
// On success the handler downstream can pull the (user, project, scope, client)
// identity bag from ctx via oauth.FromContext.
func RequireBearer(svc *oauth.Service, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractBearer(r.Header.Get("Authorization"))
			if token == "" {
				writeBearerUnauthorized(w, "invalid_request")
				return
			}
			access, err := svc.LookupActiveAccessToken(r.Context(), token)
			if err != nil {
				logger.Error("oauth bearer lookup", "err", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			if access == nil {
				writeBearerUnauthorized(w, "invalid_token")
				return
			}
			ctx := oauth.ContextWith(r.Context(), &oauth.AccessContext{
				UserID:    access.UserID,
				ProjectID: access.ProjectID,
				ClientID:  access.ClientID,
				Scope:     access.Scope,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractBearer(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func writeBearerUnauthorized(w http.ResponseWriter, errCode string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="api", error="`+errCode+`"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
