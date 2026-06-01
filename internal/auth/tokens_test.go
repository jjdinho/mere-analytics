package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestGeneratePublicToken_FormatAndLength(t *testing.T) {
	plain, hashHex, err := GeneratePublicToken()
	if err != nil {
		t.Fatalf("GeneratePublicToken: %v", err)
	}
	if !strings.HasPrefix(plain, PublicTokenPrefix) {
		t.Errorf("plaintext does not start with %q: %s", PublicTokenPrefix, plain)
	}
	if len(plain) != PublicTokenPlaintextLength {
		t.Errorf("plaintext length: got %d want %d", len(plain), PublicTokenPlaintextLength)
	}
	sum := sha256.Sum256([]byte(plain))
	if hashHex != hex.EncodeToString(sum[:]) {
		t.Errorf("hash mismatch:\n  got  %s\n  want %s", hashHex, hex.EncodeToString(sum[:]))
	}
}

func TestGeneratePublicToken_UniquePerCall(t *testing.T) {
	a, _, _ := GeneratePublicToken()
	b, _, _ := GeneratePublicToken()
	if a == b {
		t.Fatalf("two GeneratePublicToken calls returned same plaintext: %s", a)
	}
}

func TestGenerateInviteToken_FormatAndLength(t *testing.T) {
	plain, hashHex, err := GenerateInviteToken()
	if err != nil {
		t.Fatalf("GenerateInviteToken: %v", err)
	}
	if len(plain) != InviteTokenPlaintextLength {
		t.Errorf("plaintext length: got %d want %d", len(plain), InviteTokenPlaintextLength)
	}
	if strings.HasPrefix(plain, PublicTokenPrefix) {
		t.Errorf("invite token should not carry the public prefix: %s", plain)
	}
	sum := sha256.Sum256([]byte(plain))
	if hashHex != hex.EncodeToString(sum[:]) {
		t.Errorf("hash mismatch:\n  got  %s\n  want %s", hashHex, hex.EncodeToString(sum[:]))
	}
}

func TestGenerateInviteToken_UniquePerCall(t *testing.T) {
	a, _, _ := GenerateInviteToken()
	b, _, _ := GenerateInviteToken()
	if a == b {
		t.Fatalf("two GenerateInviteToken calls returned same plaintext: %s", a)
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	const in = "abcdef1234567890abcdef1234567890abcdef1234567890"
	a := HashToken(in)
	b := HashToken(in)
	if a != b {
		t.Errorf("HashToken not deterministic: %s vs %s", a, b)
	}
}
