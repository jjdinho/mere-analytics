// Package web wires the application's HTTP surface. Routes split into three
// layers: every request goes through recover → log → authMiddleware (session
// + viewer + CSRF + must_change_password gate). Authenticated-only routes
// additionally pass through requireSession; the login page passes through
// requireAnonymous. There is no public signup route — the first user is
// created by the operator via scripts/operator/create-user.sql (kamal
// create-user) and subsequent users join via invite links rendered on
// /invites/:token.
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

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static.FS()))))

	mux.Handle("GET /{$}", indexHandler(logger))

	// Auth routes available only when authMiddleware ran (svc != nil).
	if opts.AuthService != nil {
		anon := requireAnonymous()
		auth := requireSession()

		// Public + anonymous-only. There is no public /signup route —
		// the first user is created by the operator via scripts/operator/
		// create-user.sql; further users join via invite links.
		mux.Handle("GET /login", anon(getLogin(opts.AuthService, logger)))
		mux.Handle("POST /login", anon(postLogin(opts.AuthService, logger, opts.SecureCookies)))
		mux.Handle("POST /logout", postLogout(opts.AuthService, logger, opts.SecureCookies))

		// Invites: GET is public (the page adapts to session state — anon
		// visitors get an inline signup form, authed visitors get a confirm
		// button). POST is also public: an anon POST creates an account +
		// consumes the invite atomically; an authed POST just consumes.
		mux.Handle("GET /invites/{token}", getInvite(opts.AuthService, logger))
		mux.Handle("POST /invites/{token}", postInvite(opts.AuthService, logger, opts.SecureCookies))

		// Teams + projects + tokens — all require an authenticated session.
		mux.Handle("GET /teams/{id}", auth(getTeam(logger)))
		mux.Handle("POST /teams/{id}/invites", auth(postTeamInvites(logger)))
		mux.Handle("POST /teams/{id}/projects", auth(postTeamProjects(logger)))

		mux.Handle("GET /projects/{id}", auth(getProject(logger)))
		mux.Handle("POST /projects/{id}/delete", auth(postProjectDelete(logger)))
		mux.Handle("POST /projects/{id}/tokens", auth(postProjectTokens(logger)))
		mux.Handle("POST /projects/{id}/tokens/{tid}/revoke", auth(postProjectTokenRevoke(logger)))

		// Account / password change. The must_change_password gate inside
		// authMiddleware redirects flagged users here from everywhere else.
		mux.Handle("GET /account/password", auth(getAccountPassword()))
		mux.Handle("POST /account/password", auth(postAccountPassword(opts.AuthService, logger)))
	}

	var chain http.Handler = mux
	if opts.AuthService != nil {
		chain = authMiddleware(opts.AuthService, logger, opts.SecureCookies)(chain)
	}
	return recoverMiddleware(logger)(logMiddleware(logger)(chain))
}
