package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jjdinho/mere-analytics/internal/auth"
)

// csrfCookieName is the anonymous CSRF cookie used for pre-auth forms
// (login and the anon-invite signup form on /invites/:token). For
// authenticated requests the token comes from the session row via
// auth.Session.CSRFToken.
const csrfCookieName = "mere_csrf"

// authMiddleware does five things, in order, on every request:
//
//  1. Reads the mere_session cookie; on a valid session attaches it to ctx,
//     touches the session row (sliding expiry), and refreshes the cookie.
//  2. Falls back to an anonymous mere_csrf cookie for the CSRF token (issued
//     lazily so pre-auth forms — login and the anon-invite signup — can
//     carry one).
//  3. Enforces CSRF on non-GET requests to web routes. /v1/* and /mcp are
//     exempt (bearer-authed, no cookie surface).
//  4. Builds a per-request auth.Viewer for authenticated sessions and
//     attaches it to ctx so handlers can call ViewerFrom(ctx).Projects() etc.
//  5. Enforces the must_change_password gate: if the session has the flag
//     set and the request isn't to /account/password, /logout, or /static/*,
//     redirects to /account/password (Issue 4).
//
// On CSRF failure responds 403 immediately; the wrapped handler is never
// invoked.
func authMiddleware(svc *auth.Service, logger *slog.Logger, secureCookies bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			var session *auth.Session

			if cookie, err := r.Cookie(auth.SessionCookieName); err == nil && cookie.Value != "" {
				s, err := svc.LookupSession(ctx, cookie.Value)
				switch {
				case err == nil:
					if _, terr := svc.TouchSession(ctx, s); terr != nil {
						logger.Warn("session touch failed", "err", terr)
					}
					session = s
					setSessionCookie(w, s.ID, s.ExpiresAt, secureCookies)
				case errors.Is(err, auth.ErrSessionNotFound), errors.Is(err, auth.ErrSessionExpired):
					clearSessionCookie(w, secureCookies)
				default:
					logger.Error("session lookup failed", "err", err)
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
			}

			csrfToken := ""
			if session != nil {
				csrfToken = session.CSRFToken
			} else {
				csrfToken = ensureAnonCSRFCookie(w, r, secureCookies, logger)
			}

			if !methodIsSafe(r.Method) && !pathIsAPI(r.URL.Path) {
				submitted := submittedCSRFToken(r)
				if !auth.CSRFTokenEqual(submitted, csrfToken) {
					logger.Warn("csrf rejected",
						"path", r.URL.Path,
						"method", r.Method,
						"has_session", session != nil,
					)
					http.Error(w, "csrf token invalid", http.StatusForbidden)
					return
				}
			}

			ctx = auth.WithSession(ctx, session)
			ctx = auth.WithCSRFToken(ctx, csrfToken)
			if session != nil {
				ctx = auth.WithViewer(ctx, auth.NewViewer(svc, session.UserID))

				// must_change_password gate (Issue 4). Allow the password-change
				// page (so the user can resolve the flag), /logout (escape
				// hatch), and /static/* (avoid breaking the CSS reference).
				if session.MustChangePassword && !mustChangePasswordAllowed(r) {
					http.Redirect(w, r, "/account/password", http.StatusSeeOther)
					return
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// mustChangePasswordAllowed reports whether the request may proceed despite
// the user's flagged session — the change-password form itself, logout, and
// static assets. Everything else gets redirected to /account/password.
func mustChangePasswordAllowed(r *http.Request) bool {
	p := r.URL.Path
	switch p {
	case "/account/password", "/logout":
		return true
	}
	return strings.HasPrefix(p, "/static/")
}

// requireSession returns a middleware that 302-redirects to /login when no
// session is attached to ctx (i.e. authMiddleware ran but didn't find one).
// Authenticated requests pass straight through.
func requireSession() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if auth.SessionFrom(r.Context()) == nil {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireAnonymous redirects already-authenticated users away from /login
// so they don't re-submit credentials over a logged-in session.
func requireAnonymous() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if auth.SessionFrom(r.Context()) != nil {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// methodIsSafe reports whether m is an HTTP method we'd consider safe (no
// state change), per RFC 7231. CSRF enforcement is skipped for these.
func methodIsSafe(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// pathIsAPI reports whether p targets a bearer-authed surface (/v1/*,
// /api/v1/*, /mcp) or a non-cookie OAuth endpoint (/oauth/register,
// /oauth/token). Those routes carry no session cookie and so are immune to
// CSRF; checking the token would be both unnecessary and impossible.
//
// /oauth/authorize POST is deliberately NOT exempt — it's a session-bearing
// browser submission from the consent page and needs the CSRF check.
func pathIsAPI(p string) bool {
	switch p {
	case "/oauth/register", "/oauth/token":
		return true
	}
	return strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/api/v1/") || p == "/mcp" || strings.HasPrefix(p, "/mcp/")
}

// submittedCSRFToken returns the CSRF token the client supplied in this
// request, preferring the X-CSRF-Token header (htmx layout sets it globally
// via hx-headers) over a form field (plain HTML forms).
func submittedCSRFToken(r *http.Request) string {
	if t := r.Header.Get(auth.CSRFHeader); t != "" {
		return t
	}
	if err := r.ParseForm(); err == nil {
		return r.PostForm.Get(auth.CSRFFormField)
	}
	return ""
}

func setSessionCookie(w http.ResponseWriter, id string, expiresAt time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    id,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ensureAnonCSRFCookie returns the anonymous CSRF token for this request,
// generating + setting a fresh cookie when the request didn't carry one.
// The cookie stays HttpOnly (the token is embedded in HTML at render time via
// the layout's hx-headers attribute, so JS doesn't need to read it).
func ensureAnonCSRFCookie(w http.ResponseWriter, r *http.Request, secure bool, logger *slog.Logger) string {
	if c, err := r.Cookie(csrfCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	tok, err := auth.GenerateCSRFToken()
	if err != nil {
		logger.Error("csrf token generate", "err", err)
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    tok,
		Path:     "/",
		Expires:  time.Now().Add(24 * time.Hour),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return tok
}
