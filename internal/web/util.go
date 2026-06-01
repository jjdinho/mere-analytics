package web

import (
	"encoding/json"
	"errors"
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

// DecodeJSON reads the request body as JSON into T with the uniform error
// mapping the v1 endpoints expect: a body that overran MaxBody → 413, a JSON
// syntax/type error → 400. On either failure the response is fully written
// and ok=false is returned so the handler should bail. On success the
// decoded value is returned with ok=true.
//
// Pair with MaxBody upstream so r.Body is already wrapped in
// http.MaxBytesReader; this helper just catches the *http.MaxBytesError the
// reader surfaces and converts it to the right status.
func DecodeJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return v, false
		}
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return v, false
	}
	return v, true
}

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
