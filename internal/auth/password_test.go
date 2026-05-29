package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerifyPassword_roundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Errorf("VerifyPassword(correct password) = false, want true")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Errorf("VerifyPassword(wrong password) = true, want false")
	}
	if VerifyPassword("$2a$10$malformedhash", "anything") {
		t.Errorf("VerifyPassword(malformed hash) = true, want false")
	}
}

func TestValidatePassword_lengthPolicy(t *testing.T) {
	t.Run("too short", func(t *testing.T) {
		err := ValidatePassword(strings.Repeat("a", MinPasswordLength-1))
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("expected *ValidationError, got %T %v", err, err)
		}
		if ve.Field != "password" {
			t.Errorf("Field: got %q want password", ve.Field)
		}
	})
	t.Run("at boundary lower", func(t *testing.T) {
		if err := ValidatePassword(strings.Repeat("a", MinPasswordLength)); err != nil {
			t.Errorf("MinPasswordLength rejected: %v", err)
		}
	})
	t.Run("at boundary upper", func(t *testing.T) {
		if err := ValidatePassword(strings.Repeat("a", MaxPasswordLength)); err != nil {
			t.Errorf("MaxPasswordLength rejected: %v", err)
		}
	})
	t.Run("too long", func(t *testing.T) {
		err := ValidatePassword(strings.Repeat("a", MaxPasswordLength+1))
		if err == nil {
			t.Errorf("expected error for over-long password")
		}
	})
}

func TestValidateEmail(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", true},
		{"no-at", true},
		{"two@@signs", true},
		{"@nolocal.com", true},
		{"nodomain@", true},
		{"a@b.c", false},
		{"user.name+tag@example.co.uk", false},
	}
	for _, tc := range cases {
		err := ValidateEmail(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateEmail(%q): err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	cases := map[string]string{
		"  User@Example.COM  ": "user@example.com",
		"already@lower.com":    "already@lower.com",
	}
	for in, want := range cases {
		if got := NormalizeEmail(in); got != want {
			t.Errorf("NormalizeEmail(%q): got %q want %q", in, got, want)
		}
	}
}
