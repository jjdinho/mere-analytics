package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// CSRFHeader is the HTTP header htmx forwards on every request via the
// hx-headers attribute set in views/layout.templ.
const CSRFHeader = "X-CSRF-Token"

// CSRFFormField is the hidden form input emitted by the @csrfField() templ
// helper for plain (non-htmx) form submissions.
const CSRFFormField = "csrf_token"

// csrfTokenBytes is the number of random bytes per token before base64
// encoding. 32 bytes → 43 base64url chars; an attacker would need ~2^128
// guesses to forge, far past anything they could feasibly issue inside the
// 30-day session lifetime.
const csrfTokenBytes = 32

// GenerateCSRFToken returns a URL-safe random token sized for inclusion in
// HTTP headers and form fields.
func GenerateCSRFToken() (string, error) {
	var b [csrfTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("csrf: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// CSRFTokenEqual constant-time compares two CSRF tokens. Empty tokens never
// compare equal even to other empty tokens — a missing token in either side
// is a programmer error, never a match.
func CSRFTokenEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
