// Package web wires the application's HTTP surface. Routes split into three
// layers: every request goes through recover → log → authMiddleware (session
// + viewer + CSRF + must_change_password gate). Authenticated-only routes
// additionally pass through requireSession; the login page passes through
// requireAnonymous. There is no public signup route — the first user is
// created by the operator via scripts/operator/create-user.sql (kamal
// create-user) and subsequent users join via invite links rendered on
// /invites/:token.
//
// /oauth/* implements a PKCE-only OAuth 2.1 server for /mcp + /v1/query
// (handlers land in later PRs). /v1/whoami exists today as the bearer
// middleware's production smoke surface.
package web

import (
	"log/slog"
	"net/http"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/static"
)

// Options bundles the dependencies needed to build the HTTP handler.
// SecureCookies enables the Secure flag on session/csrf cookies — disabled
// in dev (plaintext http on localhost) and enabled in production via env.
//
// OAuthService and OAuthIssuer are required to wire the OAuth routes; they
// may be nil/empty in degenerate test scenarios that build a handler without
// a database. The auth-only path keeps working in that case.
type Options struct {
	AuthService   *auth.Service
	OAuthService  *oauth.Service
	OAuthIssuer   string
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

		// Teams + projects — all require an authenticated session.
		mux.Handle("GET /teams/{id}", auth(getTeam(logger)))
		mux.Handle("POST /teams/{id}/invites", auth(postTeamInvites(logger)))
		mux.Handle("POST /teams/{id}/projects", auth(postTeamProjects(logger)))

		mux.Handle("GET /projects/{id}", auth(getProject(logger)))
		mux.Handle("POST /projects/{id}/delete", auth(postProjectDelete(logger)))

		// Account / password change. The must_change_password gate inside
		// authMiddleware redirects flagged users here from everywhere else.
		mux.Handle("GET /account/password", auth(getAccountPassword()))
		mux.Handle("POST /account/password", auth(postAccountPassword(opts.AuthService, logger)))
	}

	// OAuth 2.1 + bearer-protected v1 routes.
	if opts.OAuthService != nil && opts.AuthService != nil {
		auth := requireSession()
		mux.Handle("GET /.well-known/oauth-authorization-server", getOAuthMetadata(opts.OAuthIssuer))
		mux.Handle("POST /oauth/register", postOAuthRegister(opts.OAuthService, logger))
		mux.Handle("POST /oauth/token", postOAuthToken(opts.OAuthService, logger))
		// GET /oauth/authorize must handle the unauthenticated case itself
		// (redirect to /login?next=…), so it is NOT wrapped in requireSession.
		mux.Handle("GET /oauth/authorize", getOAuthAuthorize(opts.AuthService, opts.OAuthService, logger))
		mux.Handle("POST /oauth/authorize", auth(postOAuthAuthorize(opts.AuthService, opts.OAuthService, logger)))

		// Bearer-protected production endpoint. Future /v1/query and /mcp
		// mount with the same RequireBearer pattern.
		bearer := RequireBearer(opts.OAuthService, logger)
		mux.Handle("GET /v1/whoami", bearer(getWhoami()))
	}

	var chain http.Handler = mux
	if opts.AuthService != nil {
		chain = authMiddleware(opts.AuthService, logger, opts.SecureCookies)(chain)
	}
	return recoverMiddleware(logger)(logMiddleware(logger)(chain))
}
