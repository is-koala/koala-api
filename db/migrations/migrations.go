// Package migrations embeds the goose migrations of the Koala database
// (ADR-0012). `koala migrate` applies them as koala_owner.
//
// Files are named NNNN_<name>.sql and numbered sequentially. River's own
// schema is imported as one file per River version, NNNN_river_main_vVVV.sql
// (ADR-0010); TestRiverMigrationsMatchRiver keeps them equal to the SQL of
// the River release in go.mod.
package migrations

import "embed"

// FS holds the migration files at its root.
//
//go:embed *.sql
var FS embed.FS
