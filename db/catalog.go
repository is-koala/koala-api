// Package db holds the database sources that are not migrations or roles:
// the soft-delete catalog (softdelete.yaml) and the sqlc query files
// (queries/). Import it under another name; internal/platform/db is the
// database platform.
package db

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed softdelete.yaml
var files embed.FS

//go:embed queries
var queries embed.FS

// SoftDeleteCatalog returns db/softdelete.yaml, the soft-delete catalog
// (ADR-0013, ADR-0020). Parse it with softdelete.ParseCatalog.
func SoftDeleteCatalog() []byte {
	data, err := files.ReadFile("softdelete.yaml")
	if err != nil {
		// The file is embedded at build time; reading it cannot fail.
		panic(fmt.Sprintf("db: embedded softdelete.yaml: %v", err))
	}
	return data
}

// Queries returns the sqlc query files under db/queries, rooted at that
// directory (so a path reads <area>/<file>.sql). A test reads the
// inner-by-design markers of a query from here (softdelete.MarkedParents,
// ADR-0027 §2).
func Queries() fs.FS {
	sub, err := fs.Sub(queries, "queries")
	if err != nil {
		// The directory is embedded at build time; Sub cannot fail.
		panic(fmt.Sprintf("db: embedded queries: %v", err))
	}
	return sub
}
