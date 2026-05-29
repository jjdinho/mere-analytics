package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Token lifecycle (Step 4):
//
//                 ┌───────────────────────────┐
//   GenerateToken │ plaintext ← "mere_pat_"   │  ──┐ shown to user once
//                 │             + 43 chars    │    │ via render-on-POST
//                 │ hash      ← sha256 hex    │    │ (Issue 3)
//                 └────────────┬──────────────┘    │
//                              │                   ▼
//                              │            api_tokens row
//                              │            (token_hash only)
//                              │
//                       HashToken(submitted bearer)
//                              │
//                              ▼
//                       Step 5 bearer middleware
//                       looks up by hash; project_id
//                       attaches to ctx for /v1/* + /mcp.
//
// The plaintext exists only in the response that creates it; it is never
// echoed by ListTokensForProject, never logged, never stored.

// TokenPrefix is the literal leading string of every plaintext API token.
// The prefix lets leak scanners (TruffleHog, GitHub secret scanning) flag
// accidentally committed tokens.
const TokenPrefix = "mere_pat_"

// tokenRandomBytes is the number of CSPRNG bytes per token before base64url
// encoding. 32 bytes → 43 RawURLEncoding chars → ~2^256 guess space.
const tokenRandomBytes = 32

// TokenPlaintextLength is the exact length of a valid plaintext token:
// 9 (prefix) + 43 (RawURLEncoding of 32 random bytes) = 52.
const TokenPlaintextLength = len(TokenPrefix) + 43

// GenerateToken returns the plaintext token and its sha256 hex hash. The
// caller writes hash to api_tokens.token_hash and surfaces plaintext to the
// user exactly once.
func GenerateToken() (plaintext, hashHex string, err error) {
	var b [tokenRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("token: read random: %w", err)
	}
	plaintext = TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return plaintext, HashToken(plaintext), nil
}

// HashToken returns the sha256 hex hash of plaintext. Deterministic; same
// input → same output. Used at issuance and at bearer lookup.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// LooksLikeAPIToken is a cheap structural check used by future bearer
// middleware to reject obviously-bogus inputs before a DB round-trip. Only
// checks the prefix + length; full validity requires the hash to match a row.
func LooksLikeAPIToken(s string) bool {
	return len(s) == TokenPlaintextLength && strings.HasPrefix(s, TokenPrefix)
}
