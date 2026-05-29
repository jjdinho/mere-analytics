package auth

import (
	"context"
	"time"
)

// Session is the per-request view of an authenticated user. The HTTP
// middleware loads it once per request and attaches it via WithSession.
type Session struct {
	ID                 string
	UserID             string
	UserEmail          string
	CSRFToken          string
	CreatedAt          time.Time
	ExpiresAt          time.Time
	MustChangePassword bool
}

type contextKey int

const (
	sessionContextKey   contextKey = 1
	csrfTokenContextKey contextKey = 2
)

// WithSession returns a child context carrying s. A nil s is allowed and means
// "unauthenticated"; downstream code calls SessionFrom and gets nil back.
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, sessionContextKey, s)
}

// SessionFrom returns the session attached to ctx, or nil if no session was
// attached (anonymous request).
func SessionFrom(ctx context.Context) *Session {
	v, _ := ctx.Value(sessionContextKey).(*Session)
	return v
}

// WithCSRFToken stashes the current request's CSRF token in ctx so templ
// helpers can recover it without re-reading cookies. Authenticated requests
// get session.CSRFToken; anonymous requests get the mere_csrf cookie value.
func WithCSRFToken(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, csrfTokenContextKey, tok)
}

// CSRFTokenFrom returns the request's contextual CSRF token. Returns the empty
// string only if the middleware chain that sets it didn't run — a programmer
// error, not a user-facing state.
func CSRFTokenFrom(ctx context.Context) string {
	v, _ := ctx.Value(csrfTokenContextKey).(string)
	return v
}
