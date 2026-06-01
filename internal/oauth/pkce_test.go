package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestVerifyS256(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	matching := challengeFor(verifier)

	cases := []struct {
		name      string
		verifier  string
		challenge string
		want      bool
	}{
		{"matching pair", verifier, matching, true},
		{"mismatched verifier", "different-verifier-zzz", matching, false},
		{"empty verifier", "", matching, false},
		{"empty challenge", verifier, "", false},
		{"empty both", "", "", false},
		{"oversize verifier", strings.Repeat("a", 1024), matching, false},
		{"swapped (verifier passed as challenge)", matching, verifier, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyS256(c.verifier, c.challenge); got != c.want {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}
