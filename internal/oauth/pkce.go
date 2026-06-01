package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// VerifyS256 reports whether base64url(sha256(verifier)) == challenge using a
// constant-time comparison on the digest. RFC 7636 §4.6.
//
// verifier and challenge are both base64url-encoded RFC 7636 strings (no
// padding). An empty verifier or challenge is rejected — there's no legitimate
// case where either is empty when this function is reached.
func VerifyS256(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	encoded := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(encoded), []byte(challenge)) == 1
}
