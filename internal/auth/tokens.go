package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Token lifecycle (Step 4 + public/secret split):
//
//                 ┌───────────────────────────┐
//   GenerateToken │ plaintext ← prefix(kind)  │  ──┐ shown to user
//   (kind)        │             + 43 chars    │    │ (once for secret,
//                 │ hash      ← sha256 hex    │    │  always for public)
//                 └────────────┬──────────────┘    │
//                              │                   ▼
//                              │            api_tokens row
//                              │            kind=public_ingest|secret_api
//                              │            (token_plaintext set only when
//                              │             kind=public_ingest)
//                              │
//                       HashToken(submitted bearer)
//                              │
//                              ▼
//                       Future bearer middleware
//                       looks up by hash; project_id + kind
//                       attach to ctx for /v1/* + /mcp.

// TokenKind discriminates the two bearer flavours we issue.
//
//   - TokenKindPublic: lives in the snippet on a customer's website, hence
//     non-secret. Auto-provisioned at project create, displayed verbatim on
//     the project page. Authorized only for ingest in the future bearer
//     middleware.
//   - TokenKindSecret: user-issued from the project page for /v1/* + MCP.
//     Plaintext shown exactly once at issuance (render-on-POST).
type TokenKind string

const (
	TokenKindPublic TokenKind = "public_ingest"
	TokenKindSecret TokenKind = "secret_api"
)

// PublicTokenPrefix tags the snippet/ingest token (mere_pub_…). Public by
// design — its presence in client HTML is the entire point — so the prefix
// being scanner-visible is fine.
const PublicTokenPrefix = "mere_pub_"

// SecretTokenPrefix tags read/MCP bearers (mere_pat_…). The prefix lets leak
// scanners (TruffleHog, GitHub secret scanning) flag accidentally committed
// secrets.
const SecretTokenPrefix = "mere_pat_"

// TokenPrefix is retained as an alias of SecretTokenPrefix so external callers
// that imported the original constant keep compiling. New code should pick
// the explicit Public/Secret variant.
const TokenPrefix = SecretTokenPrefix

// tokenRandomBytes is the number of CSPRNG bytes per token before base64url
// encoding. 32 bytes → 43 RawURLEncoding chars → ~2^256 guess space.
const tokenRandomBytes = 32

// TokenPlaintextLength is the exact length of a valid plaintext token of
// either kind: 9 (prefix) + 43 (RawURLEncoding of 32 random bytes) = 52.
// Both prefixes are the same length by construction.
const TokenPlaintextLength = len(SecretTokenPrefix) + 43

// prefixFor returns the plaintext prefix for kind. Unknown kinds yield "" so
// the caller's downstream LooksLike check rejects them.
func prefixFor(kind TokenKind) string {
	switch kind {
	case TokenKindPublic:
		return PublicTokenPrefix
	case TokenKindSecret:
		return SecretTokenPrefix
	}
	return ""
}

// GenerateToken returns the plaintext token and its sha256 hex hash for the
// requested kind. The caller writes hash to api_tokens.token_hash; for public
// tokens it also persists plaintext so the project page can re-display it.
func GenerateToken(kind TokenKind) (plaintext, hashHex string, err error) {
	prefix := prefixFor(kind)
	if prefix == "" {
		return "", "", fmt.Errorf("token: unknown kind %q", kind)
	}
	var b [tokenRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("token: read random: %w", err)
	}
	plaintext = prefix + base64.RawURLEncoding.EncodeToString(b[:])
	return plaintext, HashToken(plaintext), nil
}

// HashToken returns the sha256 hex hash of plaintext. Deterministic; same
// input → same output. Used at issuance and at bearer lookup.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// LooksLikeAPIToken reports whether s is structurally a token of either kind.
// Cheap pre-filter for the future bearer middleware; full validity still
// requires the hash to match a row.
func LooksLikeAPIToken(s string) bool {
	if len(s) != TokenPlaintextLength {
		return false
	}
	return strings.HasPrefix(s, PublicTokenPrefix) || strings.HasPrefix(s, SecretTokenPrefix)
}

// TokenKindOf returns the kind implied by s's prefix, or "" if the prefix
// matches neither. Length is not checked here; pair with LooksLikeAPIToken
// when full structural validity matters.
func TokenKindOf(s string) TokenKind {
	switch {
	case strings.HasPrefix(s, PublicTokenPrefix):
		return TokenKindPublic
	case strings.HasPrefix(s, SecretTokenPrefix):
		return TokenKindSecret
	}
	return ""
}
