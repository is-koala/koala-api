package media_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	dbsrc "github.com/is-koala/koala-api/db"
)

// writesBoundAt reports whether a SQL statement writes media.bound_at: an
// assignment (UPDATE … SET bound_at =, ON CONFLICT … SET) or an INSERT
// column list that names it.
func writesBoundAt(sql string) bool {
	assign := regexp.MustCompile(`(?i)\bbound_at\s*=`)
	insertColumns := regexp.MustCompile(`(?is)\binsert\s+into\s+[^(]*\([^)]*\bbound_at\b`)
	return assign.MatchString(sql) || insertColumns.MatchString(sql)
}

// TestWritesBoundAtDetector keeps the detector below honest.
func TestWritesBoundAtDetector(t *testing.T) {
	t.Parallel()

	for sql, want := range map[string]bool{
		"UPDATE active_media SET bound_at = now() WHERE id = $1":                         true,
		"update active_media set status = 'READY', bound_at=$2":                          true,
		"INSERT INTO active_media (id, owner_id, bound_at) VALUES ($1, $2, $3)":          true,
		"insert into active_media\n  (id,\n   bound_at)\nvalues ($1, $2)":                true,
		"SELECT id FROM active_media WHERE bound_at IS NULL":                             false,
		"INSERT INTO active_media (id, owner_id) VALUES ($1, $2) RETURNING id, bound_at": false,
	} {
		if got := writesBoundAt(sql); got != want {
			t.Errorf("writesBoundAt(%q) = %v, want %v", sql, got, want)
		}
	}
}

// TestOnlyBindWritesBoundAt holds media.Bind as the one writer of
// media.bound_at (ADR-0035 Karar 8). A consuming PR that binds a media in
// its own query instead of calling Bind in its binding transaction would
// skip Bind's READY check and its row lock; one that never binds leaves
// the media to the unbound sweep, which deletes it a week later.
//
// Two places are read:
//
//   - every query file of db/queries: outside the media area none may name
//     the column, and there only BindMedia may write it (an assignment or
//     an INSERT column list);
//   - every string literal of the module's non-test Go code outside the
//     generated internal/media/mediadb: hand-written SQL in Go may not
//     name the column at all.
//
// Blind spots, written down rather than guessed at: SQL assembled from
// pieces at run time (no one literal names the column), SQL read from a
// file outside db/queries, and test code, which may write fixtures. That a
// binding transaction calls Bind cannot be read from query text; it is the
// consuming handler's own test (Karar 8's obligation).
func TestOnlyBindWritesBoundAt(t *testing.T) {
	t.Parallel()

	queries := dbsrc.Queries()
	writers := 0
	err := fs.WalkDir(queries, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".sql") {
			return err
		}
		raw, err := fs.ReadFile(queries, path)
		if err != nil {
			return err
		}
		text := string(raw)
		if path != "media/media.sql" {
			if strings.Contains(text, "bound_at") {
				t.Errorf("%s names bound_at; only media.Bind (db/queries/media BindMedia) writes it", path)
			}
			return nil
		}
		for _, stmt := range strings.Split(text, "-- name: ")[1:] {
			name, _, _ := strings.Cut(stmt, " ")
			if !writesBoundAt(stmt) {
				continue
			}
			if name != "BindMedia" {
				t.Errorf("%s: query %s writes bound_at; only BindMedia may", path, name)
			}
			writers++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writers != 1 {
		t.Errorf("%d queries write bound_at, want exactly BindMedia", writers)
	}

	root := filepath.Join("..", "..")
	generated := filepath.Join(root, "internal", "media", "mediadb")
	fset := token.NewFileSet()
	files := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path == generated || (path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "bin" || name == "testdata")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(v, "bound_at") {
				t.Errorf("%s: a string literal names bound_at; hand-written SQL must not, media.Bind writes it",
					fset.Position(lit.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("read %d Go files from %s; the walk did not see the module", files, root)
	}
}
