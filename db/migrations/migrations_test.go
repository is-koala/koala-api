package migrations_test

import (
	"bufio"
	"io/fs"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/is-koala/koala-api/db/migrations"
)

var (
	fileName      = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)
	riverFileName = regexp.MustCompile(`^(\d{4})_river_main_v(\d{3})\.sql$`)
)

// TestFileNames keeps goose versions unique and in one format, so two
// branches that add the same number collide in review rather than at
// deploy time.
func TestFileNames(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations embedded")
	}
	seen := map[string]string{}
	for _, e := range entries {
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			t.Errorf("%s: want NNNN_<lower_snake>.sql", e.Name())
			continue
		}
		if prev, ok := seen[m[1]]; ok {
			t.Errorf("version %s used twice: %s and %s", m[1], prev, e.Name())
		}
		seen[m[1]] = e.Name()
	}
}

// TestRiverMigrationsMatchRiver checks that River's schema is imported into
// goose completely and verbatim (ADR-0010): every version of River's main
// migration line except 1 has a goose file, in River's order, whose Up and
// Down sections are River's SQL. Upgrading River in go.mod without
// importing its new migration fails here.
//
// Version 1 creates river_migration, River's own bookkeeping table. With an
// external migration tool River's documentation leaves it out
// (`migrate-get --exclude-version 1`) and version 5 tolerates its absence.
func TestRiverMigrationsMatchRiver(t *testing.T) {
	t.Parallel()

	migrator, err := rivermigrate.New(riverpgxv5.New(nil), &rivermigrate.Config{
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	files := riverFiles(t)
	known := map[int]bool{}
	for _, m := range migrator.AllVersions() {
		known[m.Version] = true
		name, ok := files[m.Version]
		if m.Version == 1 {
			if ok {
				t.Errorf("%s: River version 1 (river_migration) is not imported", name)
			}
			continue
		}
		if !ok {
			t.Errorf("River main migration %d has no goose file: add NNNN_river_main_v%03d.sql "+
				"from `river migrate-get --line main --version %d --up/--down` and grant any new table to koala_app",
				m.Version, m.Version, m.Version)
			continue
		}
		content, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		up, down := gooseSections(string(content))
		if normalize(up) != normalize(m.SQLUp) {
			t.Errorf("%s: Up section differs from River's version %d up SQL", name, m.Version)
		}
		if normalize(down) != normalize(m.SQLDown) {
			t.Errorf("%s: Down section differs from River's version %d down SQL", name, m.Version)
		}
	}
	for version, name := range files {
		if !known[version] {
			t.Errorf("%s: River has no main migration %d", name, version)
		}
	}
}

// riverFiles maps River version to goose file name and checks that goose
// order follows River order.
func riverFiles(t *testing.T) map[int]string {
	t.Helper()

	entries, err := fs.ReadDir(migrations.FS, ".") // sorted by name
	if err != nil {
		t.Fatal(err)
	}
	files := map[int]string{}
	last := 0
	for _, e := range entries {
		m := riverFileName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		version, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatal(err)
		}
		if version <= last {
			t.Errorf("%s: River version %d after version %d; goose order must follow River order", e.Name(), version, last)
		}
		last = version
		files[version] = e.Name()
	}
	return files
}

// gooseSections returns the Up and Down sections of a goose SQL file
// without the goose annotations.
func gooseSections(content string) (up, down string) {
	var b strings.Builder
	var section *string
	flush := func() {
		if section != nil {
			*section = b.String()
		}
		b.Reset()
	}
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := sc.Text()
		switch strings.TrimSpace(line) {
		case "-- +goose Up":
			flush()
			section = &up
			continue
		case "-- +goose Down":
			flush()
			section = &down
			continue
		}
		if section == nil || strings.HasPrefix(strings.TrimSpace(line), "-- +goose") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	flush()
	return up, down
}

// normalize removes River's schema template marker (empty schema, as
// `migrate-get` without --schema prints it) and collapses whitespace.
func normalize(sql string) string {
	sql = strings.ReplaceAll(sql, "/* TEMPLATE: schema */", "")
	return strings.Join(strings.Fields(sql), " ")
}
