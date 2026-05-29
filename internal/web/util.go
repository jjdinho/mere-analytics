package web

import (
	"net/http"
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
