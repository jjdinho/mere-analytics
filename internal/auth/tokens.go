package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Token lifecycle (post-OAuth split):
//
//                 ┌───────────────────────────┐
//   Generate*     │ plaintext ← random bytes  │  ──┐ shown to user
//                 │ hash      ← sha256 hex    │    │
//                 └────────────┬──────────────┘    │
//                              │                   ▼
//                              │            persisted row
//                              │            (token_hash; plaintext only on
//                              │             api_tokens where it is public
//                              │             by design — snippet token)
//                              │
//                       HashToken(submitted bearer)
//                              │
//                              ▼
//                       Bearer / invite lookup
//                       by hash.
//
// Two callers:
//   - GeneratePublicToken → `mere_pub_…` snippet ingest tokens (api_tokens).
//   - GenerateInviteToken → prefix-less random tokens that ride in
//     /invites/{token} URLs and live in team_invites.
// /v1/* + /mcp bearer auth uses OAuth access tokens (package internal/oauth),
// not anything generated here.

// PublicTokenPrefix tags the snippet/ingest token (mere_pub_…). Public by
// design — its presence in client HTML is the entire point — so the prefix
// being scanner-visible is fine.
const PublicTokenPrefix = "mere_pub_"

// tokenRandomBytes is the number of CSPRNG bytes per token before base64url
// encoding. 32 bytes → 43 RawURLEncoding chars → ~2^256 guess space.
const tokenRandomBytes = 32

// PublicTokenPlaintextLength is the exact length of a valid plaintext public
// ingest token: 9 (prefix) + 43 (RawURLEncoding of 32 random bytes) = 52.
const PublicTokenPlaintextLength = len(PublicTokenPrefix) + 43

// InviteTokenPlaintextLength is the length of an invite token (no prefix).
const InviteTokenPlaintextLength = 43

// GeneratePublicToken returns the plaintext snippet ingest token and its
// sha256 hex hash. The caller writes hash to api_tokens.token_hash and
// persists plaintext alongside so the project page can re-display it.
func GeneratePublicToken() (plaintext, hashHex string, err error) {
	var b [tokenRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("token: read random: %w", err)
	}
	plaintext = PublicTokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return plaintext, HashToken(plaintext), nil
}

// GenerateInviteToken returns a 43-character base64url plaintext invite token
// (no prefix) and its sha256 hex hash. Invite tokens flow through
// /invites/{token}, not the bearer middleware, so they don't need a prefix.
func GenerateInviteToken() (plaintext, hashHex string, err error) {
	var b [tokenRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("invite token: read random: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(b[:])
	return plaintext, HashToken(plaintext), nil
}

// HashToken returns the sha256 hex hash of plaintext. Deterministic; same
// input → same output. Used at issuance and at every lookup (api tokens,
// invites, oauth access tokens, oauth authorization codes).
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
