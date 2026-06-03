// Package web wires the application's HTTP surface. Routes split into three
// layers: every request goes through recover → log → authMiddleware (session
// + viewer + CSRF + must_change_password gate). Authenticated-only routes
// additionally pass through requireSession; the login page passes through
// requireAnonymous. There is no public signup route — the first user is
// created by the operator via scripts/operator/create-user.sql (kamal
// create-user) and subsequent users join via invite links rendered on
// /invites/:token.
//
// /oauth/* implements a PKCE-only OAuth 2.1 server for /mcp + /api/v1/*.
// /api/v1/whoami exists as the bearer middleware's production smoke surface.
package web

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jjdinho/mere-analytics/extension"
	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/ingest"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/query"
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

	// Version is the build-time version stamp (git describe), injected into
	// cmd/server via -ldflags and surfaced in the /healthz body so operators
	// can confirm what's deployed from `kamal logs` / a health probe. Empty in
	// most tests; "dev" for un-stamped local builds.
	Version string

	// IngestService + the three following knobs wire POST /api/v1/ingest/events.
	// Nil IngestService is supported so degenerate test scenarios that build a
	// handler without a CH pool still work — the route is just absent.
	IngestService        *ingest.Service
	AllowedOrigins       []string
	IngestMaxBodyBytes   int64
	DLQDepth503Threshold int

	QueryExecutor     *query.Executor
	QuerySchema       *query.SchemaProvider
	QueryMaxBodyBytes int64

	// MCPHandler is the Streamable HTTP transport for the /mcp endpoint
	// (internal/mcp.NewHTTPHandler). It mounts behind the same RequireBearer
	// + CORS middleware as /api/v1/*. Nil leaves /mcp unmounted — supported
	// for degenerate test scenarios built without a CH pool.
	MCPHandler http.Handler

	// RateLimiter is the extension seam consulted on the ingest and query/MCP
	// paths after the tenant is resolved (see docs/extending.md). Nil defaults to
	// the no-op extension.AllowAll, so the open-source build's behavior is
	// unchanged; a wrapper or self-hoster injects a real limiter here.
	RateLimiter extension.RateLimiter
}

// Handler builds the application's root http.Handler.
func Handler(opts Options) http.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.QueryMaxBodyBytes == 0 {
		opts.QueryMaxBodyBytes = 256 * 1024
	}
	if opts.RateLimiter == nil {
		opts.RateLimiter = extension.AllowAll{}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", healthzHandler(opts))

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
		if opts.QueryExecutor != nil && opts.QuerySchema != nil {
			mux.Handle("GET /projects/{id}/query", auth(getProjectQuery(opts.QuerySchema, logger)))
			mux.Handle("POST /projects/{id}/query", auth(MaxBody(opts.QueryMaxBodyBytes)(postProjectQuery(opts.QueryExecutor, opts.QuerySchema, logger))))
		}

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

		// Bearer-protected production endpoints. /api/v1/* and /mcp share the
		// same RequireBearer + CORS middleware so the auth + cross-origin
		// surface is identical across both front doors.
		bearer := RequireBearer(opts.OAuthService, logger)
		cors := CORS(opts.AllowedOrigins)
		mux.Handle("GET /api/v1/whoami", bearer(getWhoami()))

		// MCP: one endpoint, all methods (the Streamable HTTP transport
		// handles POST/GET/DELETE). CORS is outermost so a browser preflight
		// is answered before bearer auth rejects the credential-less OPTIONS.
		if opts.MCPHandler != nil {
			mux.Handle("/mcp", cors(bearer(rateLimit(opts.RateLimiter, "mcp")(MaxBody(opts.QueryMaxBodyBytes)(opts.MCPHandler)))))
		}

		if opts.QueryExecutor != nil && opts.QuerySchema != nil {
			queryChain := cors(bearer(rateLimit(opts.RateLimiter, "query")(MaxBody(opts.QueryMaxBodyBytes)(postAPIProjectQuery(opts.AuthService, opts.QueryExecutor, logger)))))
			schemaChain := cors(bearer(rateLimit(opts.RateLimiter, "query")(getAPIProjectSchema(opts.AuthService, opts.QuerySchema, logger))))
			mux.Handle("POST /api/v1/projects/{project_id}/query", queryChain)
			mux.Handle("GET /api/v1/projects/{project_id}/schema", schemaChain)
			mux.Handle("OPTIONS /api/v1/projects/{project_id}/query", cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})))
			mux.Handle("OPTIONS /api/v1/projects/{project_id}/schema", cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})))
		}
	}

	if opts.IngestService != nil {
		cors := CORS(opts.AllowedOrigins)
		ingestChain := cors(
			MaxBody(opts.IngestMaxBodyBytes)(
				requirePublicToken(opts.IngestService, logger)(
					rateLimit(opts.RateLimiter, "ingest")(
						postIngest(opts.IngestService, logger)))))
		mux.Handle("POST /api/v1/ingest/events", ingestChain)
		mux.Handle("OPTIONS /api/v1/ingest/events", cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})))
	}

	var chain http.Handler = mux
	if opts.AuthService != nil {
		chain = authMiddleware(opts.AuthService, logger, opts.SecureCookies)(chain)
	}
	return recoverMiddleware(logger)(logMiddleware(logger)(chain))
}

// healthzPayload is the JSON body /healthz emits. ingest_disabled rides on
// every healthy response so operators can confirm the kill switch state
// without grepping logs. depth is the in-process DLQ gauge.
type healthzPayload struct {
	Status         string `json:"status"`
	Version        string `json:"version"`
	IngestDisabled bool   `json:"ingest_disabled"`
	DLQDepth       int64  `json:"dlq_depth"`
}

// healthzHandler reports the ingest pipeline's health as JSON. Returns 503
// when the fatal flag is set OR the DLQ depth has crossed the configured
// threshold; in both cases kamal-proxy will circuit-break the instance.
// logMiddleware already skips /healthz so this is cheap to poll.
func healthzHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var (
			disabled bool
			fatal    bool
			depth    int64
		)
		if opts.IngestService != nil {
			f := opts.IngestService.Flags()
			disabled = f.IsDisabled()
			fatal = f.IsFatal()
			depth = f.DLQDepth()
		}
		status := "ok"
		code := http.StatusOK
		if fatal || (opts.DLQDepth503Threshold > 0 && depth > int64(opts.DLQDepth503Threshold)) {
			status = "down"
			code = http.StatusServiceUnavailable
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(healthzPayload{
			Status:         status,
			Version:        opts.Version,
			IngestDisabled: disabled,
			DLQDepth:       depth,
		})
	}
}
