package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantOut   string
		wantError bool
	}{
		{"plain", "Production", "Production", false},
		{"leading + trailing whitespace trimmed", "  Production  ", "Production", false},
		{"empty", "", "", true},
		{"whitespace only", "   ", "", true},
		{"at max length", strings.Repeat("a", MaxNameLength), strings.Repeat("a", MaxNameLength), false},
		{"over max length", strings.Repeat("a", MaxNameLength+1), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ValidateName("Project", tt.in)
			if tt.wantError {
				if err == nil {
					t.Errorf("want error, got out=%q nil err", out)
				}
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Errorf("error is not *ValidationError: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("want no error, got %v", err)
			}
			if out != tt.wantOut {
				t.Errorf("trimmed: got %q want %q", out, tt.wantOut)
			}
		})
	}
}

func TestValidateName_LabelInMessage(t *testing.T) {
	_, err := ValidateName("Token", "")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %v", err)
	}
	if !strings.Contains(ve.Msg, "Token") {
		t.Errorf("error message %q missing field label", ve.Msg)
	}
}
