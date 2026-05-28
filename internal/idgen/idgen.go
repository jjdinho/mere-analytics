// Package idgen is the single source of truth for ID generation across the app.
// All IDs are UUID v7 (time-sortable, RFC 9562). Changing the format means
// changing this one file.
package idgen

import "github.com/google/uuid"

// New returns a new UUID v7 as its canonical lowercase 36-char string form.
func New() string { return uuid.Must(uuid.NewV7()).String() }
