package web

import (
	"net/http"
	"strings"
	"time"
)

// absoluteURL builds an absolute https/http URL from r's host + a path. Used
// when rendering invite links so the user can copy-paste them out-of-band.
func absoluteURL(r *http.Request, path string) string {
	scheme := "https"
	if r.TLS == nil {
		// Behind kamal-proxy in production, r.TLS will be nil but the proxy
		// terminates TLS — honor the X-Forwarded-Proto header it sets. Dev
		// (./scripts/dev) sets neither; falls through to "http".
		if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host + path
}

// nowOrSvcNow returns the current time. Exists as a thin seam so a future
// test can swap in a deterministic clock by intercepting via context; today
// it's just time.Now. Pulled out of the handler bodies for readability.
func nowOrSvcNow(_ *http.Request) time.Time { return time.Now() }

// safeRedirect reports whether the given post-login `next` target is a
// same-origin path safe to redirect to without enabling an open-redirect or
// re-entrant /login loop. Accepts only paths that start with a single "/"
// and do not begin with "//" or "/\" (protocol-relative URL bypasses).
// Schemes (javascript:, http:, etc.) are rejected by the leading-slash check.
// Paths under /login are also rejected so a malicious next= can't loop a user
// back through the login form.
func safeRedirect(s string) bool {
	if s == "" {
		return false
	}
	if !strings.HasPrefix(s, "/") {
		return false
	}
	if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "/\\") {
		return false
	}
	if s == "/login" || strings.HasPrefix(s, "/login?") || strings.HasPrefix(s, "/login/") {
		return false
	}
	return true
}
