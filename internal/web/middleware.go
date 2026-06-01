package web

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// statusRecorder captures the response status for logging without buffering
// the body. http.Handler implementations write status via WriteHeader; for
// handlers that skip WriteHeader entirely, the implicit 200 is captured by
// initializing status to 200.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logMiddleware logs every request at info on completion. /healthz is skipped
// to avoid drowning logs in kamal-proxy's 3-second healthcheck poll.
func logMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		})
	}
}

// MaxBody wraps the request body in http.MaxBytesReader so reads past n
// bytes fail with *http.MaxBytesError. DecodeJSON below maps that error to
// 413; raw-body handlers can do the same with errors.As.
//
// Mount per-route — different surfaces have different ceilings (ingest 10
// MiB; future /v1/query much smaller).
func MaxBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

// CORS handles the SDK snippet's cross-origin POST to /v1/ingest. An empty
// allowedOrigins slice is "permissive" — the response carries
// Access-Control-Allow-Origin: * so any first-party page can write events.
// A non-empty slice is the operator-restricted form: the request Origin must
// match exactly or the CORS headers are omitted (the browser then blocks the
// response from JS, which is the only enforcement layer that matters).
//
// Allow-Methods / Allow-Headers are static — we don't accept cookies or
// custom verbs on /v1/ingest.
//
// Preflight (OPTIONS + Access-Control-Request-Method) short-circuits with
// 204; the wrapped handler never runs.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	allow := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allow[o] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			setCORSHeaders(w, origin, allow)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func setCORSHeaders(w http.ResponseWriter, origin string, allow map[string]struct{}) {
	if len(allow) == 0 {
		// Permissive default: any caller. The snippet is hosted on
		// arbitrary first-party domains, so * is the right floor.
		w.Header().Set("Access-Control-Allow-Origin", "*")
	} else if origin != "" {
		if _, ok := allow[origin]; ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		} else {
			// Origin not on the list — omit the header so the browser
			// blocks the response.
			return
		}
	} else {
		// No Origin header + restricted list = nothing to echo; bail.
		return
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
}

// recoverMiddleware turns panics into 500 responses + logged stack traces.
func recoverMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic",
						"path", r.URL.Path,
						"err", rec,
						"stack", string(debug.Stack()),
					)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
