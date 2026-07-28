// Package migrations holds this service's schema migrations and embeds them into
// the binary that applies them, so that `tehran db migrate` needs nothing on disk
// beside it and the image being deployed carries exactly the schema it expects.
//
// Files are named <timestamp>_<name>.sql — see the migrate-new target in the
// Makefile. Timestamps rather than a counter, deliberately: two branches each
// adding "the next" sequential number merge cleanly and then collide at run time,
// where the collision is a duplicate version rather than a conflict anybody saw
// in review.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed *.sql
var files embed.FS

// FS returns the embedded migrations, rooted at the .sql files themselves, which
// is where goose globs — it does not recurse, so a caller that reaches for the
// embed.FS of a subdirectory has to fs.Sub it first. Declared beside the
// migrations, this one needs no such thing.
func FS() fs.FS { return files }
