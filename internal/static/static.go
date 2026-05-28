// Package static owns the embedded static-asset filesystem. Lives in its
// own package so the //go:embed directive sits next to the files it embeds
// (Go forbids ../ in embed paths).
package static

import (
	"embed"
	"io/fs"
)

//go:embed *
var rawFS embed.FS

// FS returns the static asset filesystem. README.md is included for now
// because Go's embed requires at least one matching file; once real assets
// land in step 1+ it can be removed.
func FS() fs.FS { return rawFS }
