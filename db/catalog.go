// Package db holds the database sources that are not migrations or roles:
// the soft-delete catalog (softdelete.yaml) and the sqlc query files
// (queries/). Import it under another name; internal/platform/db is the
// database platform.
package db

import (
	"embed"
	"fmt"
)

//go:embed softdelete.yaml
var files embed.FS

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
