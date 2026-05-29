package auth

import (
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// BcryptCost is bcrypt's work factor. 10 is Go's package default; matches the
// `bf` cost used by pgcrypto's gen_salt('bf', 10) in scripts/operator/reset-password.sql,
// so app-hashed and operator-hashed passwords sit on the same cost curve.
const BcryptCost = 10

// MinPasswordLength is the minimum length we accept at signup / password
// change. Short enough to not annoy users, long enough that an offline brute
// force on a leaked bcrypt hash is uneconomical.
const MinPasswordLength = 12

// MaxPasswordLength is bcrypt's hard ceiling: bytes 73+ are silently
// truncated, so a 100-char password and the same prefix at 72 chars produce
// identical hashes. Reject longer inputs at the boundary instead.
const MaxPasswordLength = 72

// HashPassword returns the bcrypt hash of plaintext at the package's configured cost.
func HashPassword(plaintext string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plaintext), BcryptCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt generate: %w", err)
	}
	return string(h), nil
}

// VerifyPassword reports whether plaintext matches hash.
// Any error from bcrypt (mismatch, malformed hash) returns false.
func VerifyPassword(hash, plaintext string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) == nil
}

// ValidatePassword applies the length policy without hashing.
// Returns a *ValidationError suitable for surfacing in the signup form;
// callers use errors.As to recover the user-safe message.
func ValidatePassword(plaintext string) error {
	if len(plaintext) < MinPasswordLength {
		return &ValidationError{Field: "password", Msg: fmt.Sprintf("Password must be at least %d characters.", MinPasswordLength)}
	}
	if len(plaintext) > MaxPasswordLength {
		return &ValidationError{Field: "password", Msg: fmt.Sprintf("Password must be at most %d characters.", MaxPasswordLength)}
	}
	return nil
}

// NormalizeEmail trims whitespace and lowercases the local + domain parts.
// The users_email_lower_idx in 0001_init.up.sql is on lower(email), so all
// lookups go through this helper to keep app and index aligned.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidateEmail does the bare-minimum syntactic check: contains exactly one '@'
// with non-empty local and domain parts. We deliberately don't validate against
// RFC 5322 — that's a swamp, and bounce-on-send is the source of truth.
func ValidateEmail(s string) error {
	if s == "" {
		return &ValidationError{Field: "email", Msg: "Email is required."}
	}
	if len(s) > 254 {
		return &ValidationError{Field: "email", Msg: "Email is too long."}
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 || strings.IndexByte(s[at+1:], '@') != -1 {
		return &ValidationError{Field: "email", Msg: "Email is not valid."}
	}
	return nil
}

// ValidationError carries a field name and user-safe message returned from
// the Validate* helpers. Handlers use errors.As(err, &*ValidationError) to
// surface Msg directly to the form; everything else logs and returns 500.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string { return e.Msg }
