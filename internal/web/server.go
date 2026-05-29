// Package web wires the application's HTTP surface. Routes split into two
// layers: every request goes through recover → log → authMiddleware
// (session + CSRF). Authenticated-only routes additionally pass through
// requireSession; the login/signup pages pass through requireAnonymous.
package web

import (
	"log/slog"
	"net/http"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/static"
)

// Options bundles the dependencies needed to build the HTTP handler.
// SecureCookies enables the Secure flag on session/csrf cookies — disabled
// in dev (plaintext http on localhost) and enabled in production via env.
type Options struct {
	AuthService   *auth.Service
	Logger        *slog.Logger
	SecureCookies bool
}

// Handler builds the application's root http.Handler.
func Handler(opts Options) http.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.Handle("GET /{$}", indexHandler())

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static.FS()))))

	// Auth routes available only when authMiddleware ran (svc != nil).
	if opts.AuthService != nil {
		anon := requireAnonymous()
		mux.Handle("GET /signup", anon(getSignup()))
		mux.Handle("POST /signup", anon(postSignup(opts.AuthService, logger, opts.SecureCookies)))
		mux.Handle("GET /login", anon(getLogin()))
		mux.Handle("POST /login", anon(postLogin(opts.AuthService, logger, opts.SecureCookies)))
		mux.Handle("POST /logout", postLogout(opts.AuthService, logger, opts.SecureCookies))
	}

	var chain http.Handler = mux
	if opts.AuthService != nil {
		chain = authMiddleware(opts.AuthService, logger, opts.SecureCookies)(chain)
	}
	return recoverMiddleware(logger)(logMiddleware(logger)(chain))
}
