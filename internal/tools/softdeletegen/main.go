// Command softdeletegen writes the generated parts of the soft-delete
// platform (ADR-0020). `make generate` runs it from the repository root:
//
//   - In every goose migration it expands the soft-delete directives into
//     SQL blocks: the columns, CHECK, index, active_<t> view and grants of a
//     soft-deleted table. The SQL lands in the migration file itself, so
//     sqlc's static catalog sees the views and review sees the DDL.
//   - It writes the rule lines of sqlc.yaml's vet rules: no-base-table from
//     db/softdelete.yaml, no-soft-delete-columns from its fixed patterns.
//
// With -print-rule <name> it prints that rule for the catalogs given as
// arguments instead (scripts/sqlc-vet-probe.sh).
//
// A directive is a line
//
//	-- +koala soft-delete v1 <table> [security_barrier]
//	-- +koala soft-delete-view v1 <table> [security_barrier]
//
// followed by the block the tool writes between
// "-- +koala begin generated" and "-- +koala end generated". The version
// names a frozen template: applied migrations never change when a template
// does, a new template gets a new version.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// Paths relative to the repository root.
const (
	catalogPath = "db/softdelete.yaml"
	sqlcPath    = "sqlc.yaml"
)

// migrationDirs hold the files whose directives are expanded: the real
// migrations and the example migrations of the test databases.
func migrationDirs() []string {
	return []string{"db/migrations", "internal/platform/dbtest/testdata/migrations"}
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "softdeletegen:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("softdeletegen", flag.ContinueOnError)
	printRule := fs.String("print-rule", "", "print the named vet rule for the catalog files given as arguments")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *printRule != "" {
		cat, err := loadCatalog(fs.Args()...)
		if err != nil {
			return err
		}
		rule, ok := rules(cat)[*printRule]
		if !ok {
			return fmt.Errorf("unknown rule %q", *printRule)
		}
		if _, err := fmt.Fprintln(os.Stdout, rule); err != nil {
			return fmt.Errorf("print rule: %w", err)
		}
		return nil
	}
	if fs.NArg() > 0 {
		return errors.New("unexpected arguments; run from the repository root without arguments")
	}

	for _, dir := range migrationDirs() {
		files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
		if err != nil {
			return fmt.Errorf("list %s: %w", dir, err)
		}
		for _, f := range files {
			if err := rewrite(f, expand); err != nil {
				return err
			}
		}
	}
	cat, err := loadCatalog(catalogPath)
	if err != nil {
		return err
	}
	return rewrite(sqlcPath, func(src string) (string, error) {
		for name, rule := range rules(cat) {
			var err error
			if src, err = writeRule(src, name, rule); err != nil {
				return "", err
			}
		}
		return src, nil
	})
}

// rules maps sqlc.yaml's generated vet rules to their CEL expressions.
func rules(cat *softdelete.Catalog) map[string]string {
	return map[string]string{
		softdelete.RuleNoBaseTable:         softdelete.NoBaseTableRule(cat),
		softdelete.RuleNoSoftDeleteColumns: softdelete.NoSoftDeleteColumnsRule(),
	}
}

func loadCatalog(paths ...string) (*softdelete.Catalog, error) {
	if len(paths) == 0 {
		return nil, errors.New("no catalog files")
	}
	docs := make([][]byte, len(paths))
	for i, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read catalog: %w", err)
		}
		docs[i] = data
	}
	cat, err := softdelete.ParseCatalog(docs...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(paths, ", "), err)
	}
	return cat, nil
}

// rewrite applies fn to the file and writes it back only when it changed.
func rewrite(path string, fn func(string) (string, error)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	out, err := fn(string(data))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if bytes.Equal(data, []byte(out)) {
		return nil
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}
