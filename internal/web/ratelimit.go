package web

import (
	"math"
	"net"
	"net/http"
	"strconv"

	"github.com/jjdinho/mere-analytics/extension"
	"github.com/jjdinho/mere-analytics/internal/oauth"
)

// rateLimit is the extension.RateLimiter call site: a func(http.Handler)
// http.Handler in the existing middleware shape. It must be mounted *after* the
// tenant is resolved so LimitKey.ProjectID is populated — on the ingest chain
// after requirePublicToken, on the query/MCP chains after RequireBearer. The
// identity fields are read back out of the context those middlewares populated:
//
//   - "ingest"        → projectFromCtx (the resolved mere_pub_ project)
//   - "query" / "mcp" → oauth.FromContext (the bearer grant's user + project)
//
// On deny it writes 429 + Retry-After (ceil seconds, omitted when the hint is
// zero), mirroring the existing saturation 503 shape so clients already handle
// it, and the wrapped handler never runs. The request has already passed auth
// here, so a 429 reveals nothing a 200/202 wouldn't.
func rateLimit(limiter extension.RateLimiter, surface string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, retryAfter := limiter.Allow(r.Context(), limitKey(r, surface))
			if !ok {
				if retryAfter > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
				}
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// limitKey assembles the extension.LimitKey from whatever the upstream auth
// middleware stashed on the context for this surface. TokenID is left empty —
// the core does not currently thread a credential id onto the context, and the
// contract permits a limiter to use only the fields it needs.
func limitKey(r *http.Request, surface string) extension.LimitKey {
	key := extension.LimitKey{Surface: surface, RemoteIP: remoteIP(r)}
	if surface == "ingest" {
		key.ProjectID = projectFromCtx(r.Context())
		return key
	}
	if ac := oauth.FromContext(r.Context()); ac != nil {
		key.ProjectID = ac.ProjectID
		key.UserID = ac.UserID
	}
	return key
}

// remoteIP returns the host portion of r.RemoteAddr, falling back to the raw
// value when it carries no port.
func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
