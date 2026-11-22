package media_test

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	dbsrc "github.com/is-koala/koala-api/db"
)

// TestOnlyBindWritesBoundAt holds media.Bind as the one writer of
// media.bound_at (ADR-0035 Karar 8). A consuming PR that binds a media in
// its own query instead of calling Bind in its binding transaction would
// skip Bind's READY check and its row lock; one that never binds leaves
// the media to the unbound sweep, which deletes it a week later. The
// first is caught here: no query file but the media area's may name the
// column, and there only BindMedia may assign it. The second needs the
// consuming handler's own test (Karar 8's obligation); the query text
// cannot show that a binding transaction calls Bind.
func TestOnlyBindWritesBoundAt(t *testing.T) {
	t.Parallel()

	queries := dbsrc.Queries()
	assigns := regexp.MustCompile(`(?i)\bbound_at\s*=`)
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
			if !assigns.MatchString(stmt) {
				continue
			}
			if name != "BindMedia" {
				t.Errorf("%s: query %s assigns bound_at; only BindMedia may", path, name)
			}
			writers++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writers != 1 {
		t.Errorf("%d queries assign bound_at, want exactly BindMedia", writers)
	}
}
