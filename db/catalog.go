// Package db holds the database sources that are not migrations or roles:
// the soft-delete catalog (softdelete.yaml) and the sqlc query files
// (queries/). Import it under another name; internal/platform/db is the
// database platform.
package db

import (
	"embed"
	"fmt"
	"io/fs"
	"regexp"
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
// inner-by-design markers of a query from here (softdelete.MarkedEdges,
// ADR-0027 §2).
func Queries() fs.FS {
	sub, err := fs.Sub(queries, "queries")
	if err != nil {
		// The directory is embedded at build time; Sub cannot fail.
		panic(fmt.Sprintf("db: embedded queries: %v", err))
	}
	return sub
}

// queryName starts one sqlc query.
var queryName = regexp.MustCompile(`(?m)^-- name: (\w+)`)

// Query is one sqlc query: its name and its text, from its `-- name:` line
// to the next one.
type Query struct {
	// File is the path the query was read from, for a test's message.
	File string
	Name string
	SQL  string
}

// SliceQueries cuts one query file into its queries. It is the one place
// that slices them: the join lint reads a slice and so does the rule that
// ties the inner-by-design marker to the generated JOIN test
// (softdelete.MarkedEdges, ADR-0027 §2). Two slicings that drift apart
// would loosen that tie without any test noticing.
func SliceQueries(file, data string) []Query {
	bounds := queryName.FindAllStringSubmatchIndex(data, -1)
	out := make([]Query, 0, len(bounds))
	for i, b := range bounds {
		end := len(data)
		if i+1 < len(bounds) {
			end = bounds[i+1][0]
		}
		out = append(out, Query{File: file, Name: data[b[2]:b[3]], SQL: data[b[0]:end]})
	}
	return out
}

// AllQueries returns every query of every file under fsys, sliced by
// SliceQueries. Pass Queries() for the embedded files or os.DirFS over
// db/queries for the files on disk; TestEmbeddedQueriesMatchTheDirectory
// holds the two equal.
func AllQueries(fsys fs.FS) ([]Query, error) {
	files, err := fs.Glob(fsys, "*/*.sql")
	if err != nil {
		return nil, fmt.Errorf("db: glob queries: %w", err)
	}
	var out []Query
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, fmt.Errorf("db: read %s: %w", f, err)
		}
		out = append(out, SliceQueries(f, string(data))...)
	}
	return out, nil
}
