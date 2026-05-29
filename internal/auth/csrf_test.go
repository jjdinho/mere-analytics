package auth

import (
	"strings"
	"testing"
)

func TestGenerateCSRFToken_uniqueAndShaped(t *testing.T) {
	a, err := GenerateCSRFToken()
	if err != nil {
		t.Fatalf("GenerateCSRFToken: %v", err)
	}
	b, err := GenerateCSRFToken()
	if err != nil {
		t.Fatalf("GenerateCSRFToken: %v", err)
	}
	if a == b {
		t.Errorf("two successive tokens were equal: %q", a)
	}
	if len(a) < 32 {
		t.Errorf("token suspiciously short: %d chars", len(a))
	}
	if strings.ContainsAny(a, "+/=\n ") {
		t.Errorf("token contains chars not in base64url alphabet: %q", a)
	}
}

func TestCSRFTokenEqual(t *testing.T) {
	tok, _ := GenerateCSRFToken()
	if !CSRFTokenEqual(tok, tok) {
		t.Errorf("equal tokens compared unequal")
	}
	if CSRFTokenEqual(tok, tok+"x") {
		t.Errorf("differing-by-one-char tokens compared equal")
	}
	if CSRFTokenEqual("", "") {
		t.Errorf("empty tokens compared equal (must fail-closed)")
	}
	if CSRFTokenEqual(tok, "") {
		t.Errorf("token vs empty compared equal")
	}
}
