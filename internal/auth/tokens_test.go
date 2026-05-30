package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestGenerateToken_SecretFormatAndLength(t *testing.T) {
	plain, hashHex, err := GenerateToken(TokenKindSecret)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(plain, SecretTokenPrefix) {
		t.Errorf("plaintext does not start with %q: %s", SecretTokenPrefix, plain)
	}
	if len(plain) != TokenPlaintextLength {
		t.Errorf("plaintext length: got %d want %d", len(plain), TokenPlaintextLength)
	}
	if !LooksLikeAPIToken(plain) {
		t.Errorf("LooksLikeAPIToken false for freshly generated secret token: %s", plain)
	}
	if got := TokenKindOf(plain); got != TokenKindSecret {
		t.Errorf("TokenKindOf: got %q want %q", got, TokenKindSecret)
	}
	sum := sha256.Sum256([]byte(plain))
	if hashHex != hex.EncodeToString(sum[:]) {
		t.Errorf("hash mismatch:\n  got  %s\n  want %s", hashHex, hex.EncodeToString(sum[:]))
	}
}

func TestGenerateToken_PublicFormatAndLength(t *testing.T) {
	plain, hashHex, err := GenerateToken(TokenKindPublic)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(plain, PublicTokenPrefix) {
		t.Errorf("plaintext does not start with %q: %s", PublicTokenPrefix, plain)
	}
	if len(plain) != TokenPlaintextLength {
		t.Errorf("plaintext length: got %d want %d", len(plain), TokenPlaintextLength)
	}
	if !LooksLikeAPIToken(plain) {
		t.Errorf("LooksLikeAPIToken false for freshly generated public token: %s", plain)
	}
	if got := TokenKindOf(plain); got != TokenKindPublic {
		t.Errorf("TokenKindOf: got %q want %q", got, TokenKindPublic)
	}
	sum := sha256.Sum256([]byte(plain))
	if hashHex != hex.EncodeToString(sum[:]) {
		t.Errorf("hash mismatch:\n  got  %s\n  want %s", hashHex, hex.EncodeToString(sum[:]))
	}
}

func TestGenerateToken_UnknownKind_Errors(t *testing.T) {
	_, _, err := GenerateToken(TokenKind("nope"))
	if err == nil {
		t.Fatal("expected error for unknown kind, got nil")
	}
}

func TestGenerateToken_UniquePerCall(t *testing.T) {
	a, _, _ := GenerateToken(TokenKindSecret)
	b, _, _ := GenerateToken(TokenKindSecret)
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
		{"correct secret prefix and length", SecretTokenPrefix + strings.Repeat("a", 43), true},
		{"correct public prefix and length", PublicTokenPrefix + strings.Repeat("a", 43), true},
		{"correct secret prefix wrong length", SecretTokenPrefix + "abc", false},
		{"correct public prefix wrong length", PublicTokenPrefix + "abc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LooksLikeAPIToken(tt.in); got != tt.want {
				t.Errorf("got %v want %v for %q", got, tt.want, tt.in)
			}
		})
	}
}

func TestTokenKindOf(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want TokenKind
	}{
		{"public prefix", PublicTokenPrefix + "x", TokenKindPublic},
		{"secret prefix", SecretTokenPrefix + "x", TokenKindSecret},
		{"unknown prefix", "garbage_xyz", TokenKind("")},
		{"empty", "", TokenKind("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TokenKindOf(tt.in); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}
