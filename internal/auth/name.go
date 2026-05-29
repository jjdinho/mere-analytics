package auth

import (
	"fmt"
	"strings"
)

// MaxNameLength caps user-supplied names (team / project / token). Long
// enough for readable descriptions, short enough to keep DB rows and UI
// rendering bounded. Plan Issue 11.
const MaxNameLength = 100

// ValidateName applies the shared name policy: trim whitespace, non-empty,
// length cap. Returns the trimmed value on success so callers don't need to
// re-trim before persisting.
//
// fieldLabel is the user-facing field name ("Team", "Project", "Token"); it
// gets embedded in the validation error message rendered back to the form.
func ValidateName(fieldLabel, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", &ValidationError{Field: "name", Msg: fmt.Sprintf("%s name is required.", fieldLabel)}
	}
	if len(trimmed) > MaxNameLength {
		return "", &ValidationError{Field: "name", Msg: fmt.Sprintf("%s name must be at most %d characters.", fieldLabel, MaxNameLength)}
	}
	return trimmed, nil
}
