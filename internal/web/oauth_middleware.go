package web

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jjdinho/mere-analytics/internal/oauth"
)

// markAccessTokenUsedTimeout bounds the fire-and-forget UPDATE so a stuck
// PG never holds a goroutine open. Two seconds is well clear of healthy
// latency and short enough that even pgPool.Close() during SIGTERM phase 3
// only waits a bounded amount on stragglers.
const markAccessTokenUsedTimeout = 2 * time.Second

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
			// Fire-and-forget last_used_at stamp. The UPDATE has a 60s
			// throttle predicate so a hot token only writes the column at
			// most once per minute; the bounded context keeps a stuck PG
			// from holding the goroutine open during shutdown.
			go func(id string) {
				ctx, cancel := context.WithTimeout(context.Background(), markAccessTokenUsedTimeout)
				defer cancel()
				if err := svc.MarkAccessTokenUsed(ctx, id); err != nil {
					logger.Warn("oauth last_used_at update", "err", err)
				}
			}(access.ID)
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
