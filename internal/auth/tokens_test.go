package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestGenerateToken_FormatAndLength(t *testing.T) {
	plain, hashHex, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) {
		t.Errorf("plaintext does not start with %q: %s", TokenPrefix, plain)
	}
	if len(plain) != TokenPlaintextLength {
		t.Errorf("plaintext length: got %d want %d", len(plain), TokenPlaintextLength)
	}
	if !LooksLikeAPIToken(plain) {
		t.Errorf("LooksLikeAPIToken false for freshly generated token: %s", plain)
	}
	// hash is sha256 hex of the plaintext.
	sum := sha256.Sum256([]byte(plain))
	if hashHex != hex.EncodeToString(sum[:]) {
		t.Errorf("hash mismatch:\n  got  %s\n  want %s", hashHex, hex.EncodeToString(sum[:]))
	}
}

func TestGenerateToken_UniquePerCall(t *testing.T) {
	a, _, _ := GenerateToken()
	b, _, _ := GenerateToken()
	if a == b {
		t.Fatalf("two GenerateToken calls returned same plaintext: %s", a)
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	const in = "mere_pat_abcdef1234567890abcdef1234567890abcdef1234567890"
	a := HashToken(in)
	b := HashToken(in)
	if a != b {
		t.Errorf("HashToken not deterministic: %s vs %s", a, b)
	}
}

func TestLooksLikeAPIToken(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"too short", "mere_pat_abc", false},
		{"missing prefix", strings.Repeat("a", TokenPlaintextLength), false},
		{"correct length & prefix", TokenPrefix + strings.Repeat("a", 43), true},
		{"correct prefix wrong length", TokenPrefix + "abc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LooksLikeAPIToken(tt.in); got != tt.want {
				t.Errorf("got %v want %v for %q", got, tt.want, tt.in)
			}
		})
	}
}
