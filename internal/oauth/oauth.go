// Package oauth implements a hand-rolled OAuth 2.1 authorization server for
// the analytics API + MCP surfaces. PKCE-only authorization-code flow, opaque
// tokens, dynamic client registration (RFC 7591), authorization-server
// metadata discovery (RFC 8414). No refresh tokens; clients re-authorize on
// expiry.
//
// Tokens are persisted hash-only via auth.HashToken (sha256 hex), reusing the
// existing token-hashing primitive. Authorization codes are one-shot enforced
// in a transaction so a parallel /oauth/token call can never double-spend.
//
// Lifecycle:
//
//	register → authorize (consent + project pick) → token → bearer-auth
//	  client_id ←   code  ←──────────────────────────  access_token (1h)
//
// The package itself is request-agnostic — HTTP wiring lives in
// internal/web/oauth_handlers.go and oauth_middleware.go.
package oauth

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jjdinho/mere-analytics/internal/postgres/db"
)

// ScopeAPI is the only scope on day one — covers /v1/* + /mcp. Clients pass
// it explicitly in /oauth/authorize; the server validates it on the way in.
const ScopeAPI = "api"

// CodeChallengeMethodS256 is the only PKCE challenge method we accept. RFC
// 7636 §4.2; spec-mandated for OAuth 2.1.
const CodeChallengeMethodS256 = "S256"

// DefaultAccessTokenTTL is the access-token lifetime when none is configured.
// Production callers set Service.AccessTokenTTL from env; tests use the
// default unless they need an expiry-edge scenario.
const DefaultAccessTokenTTL = 1 * time.Hour

// DefaultAuthorizationCodeTTL is the authorization-code lifetime. 10 minutes
// matches MCP-spec guidance — long enough to survive a slow user-agent
// roundtrip, short enough to bound exposure.
const DefaultAuthorizationCodeTTL = 10 * time.Minute

// AccessContext is the bag of identity claims attached to authenticated
// requests via RequireBearer. Handlers downstream of the middleware can pull
// it back out of ctx via FromContext.
type AccessContext struct {
	UserID    string
	ProjectID string
	ClientID  string
	Scope     string
}

type ctxKey struct{}

// ContextWith returns ctx augmented with ac. RequireBearer calls it after a
// successful token lookup.
func ContextWith(ctx context.Context, ac *AccessContext) context.Context {
	return context.WithValue(ctx, ctxKey{}, ac)
}

// FromContext returns the access context attached by RequireBearer, or nil if
// the request never carried a valid bearer token.
func FromContext(ctx context.Context) *AccessContext {
	ac, _ := ctx.Value(ctxKey{}).(*AccessContext)
	return ac
}

// Service is the package's stateful entrypoint. It bundles the pgx pool with
// the sqlc queries handle and exposes Issue/Consume/Lookup operations the
// HTTP handlers compose. now() is overridable for deterministic expiry tests.
type Service struct {
	pool                  *pgxpool.Pool
	queries               *db.Queries
	now                   func() time.Time
	AccessTokenTTL        time.Duration
	AuthorizationCodeTTL  time.Duration
}

// NewService builds a Service bound to pool with default TTLs. Callers tweak
// AccessTokenTTL / AuthorizationCodeTTL after construction to honor env-driven
// overrides; tests reach for SetNow.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{
		pool:                 pool,
		queries:              db.New(pool),
		now:                  time.Now,
		AccessTokenTTL:       DefaultAccessTokenTTL,
		AuthorizationCodeTTL: DefaultAuthorizationCodeTTL,
	}
}

// Now returns the Service's current time. Production callers get time.Now;
// tests that swap the clock via SetNow observe their override.
func (s *Service) Now() time.Time { return s.now() }

// SetNow swaps the Service's clock. Test-only.
func (s *Service) SetNow(fn func() time.Time) { s.now = fn }
