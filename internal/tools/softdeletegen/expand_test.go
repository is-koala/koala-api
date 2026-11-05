package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const migration = `-- +goose Up
CREATE TABLE notes (id uuid PRIMARY KEY);
-- +koala soft-delete v1 notes security_barrier

-- +goose Down
DROP VIEW active_notes;
DROP TABLE notes;`

func TestExpandWritesTheBlock(t *testing.T) {
	t.Parallel()

	got, err := expand(migration)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"-- +koala soft-delete v1 notes security_barrier\n" + beginMarker + "\nALTER TABLE notes\n",
		"ADD COLUMN deletion_id uuid REFERENCES deletions (id),",
		"ADD CONSTRAINT notes_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));",
		"CREATE INDEX notes_deletion_id_idx ON notes (deletion_id) WHERE deletion_id IS NOT NULL;",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON notes TO koala_maint;",
		"CREATE VIEW active_notes WITH (security_barrier) AS\n    SELECT * FROM notes WHERE deleted_at IS NULL;",
		"GRANT SELECT, INSERT, UPDATE ON active_notes TO koala_app;\n" + endMarker + "\n\n-- +goose Down",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expanded migration lacks %q:\n%s", want, got)
		}
	}

	again, err := expand(got)
	if err != nil {
		t.Fatal(err)
	}
	if again != got {
		t.Errorf("expand is not idempotent:\n%s", again)
	}
}

// A hand edit inside a block is overwritten, which makes gen-check fail on
// it.
func TestExpandReplacesAnEditedBlock(t *testing.T) {
	t.Parallel()

	clean, err := expand(migration)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(clean, "TO koala_app;", "TO koala_app, koala_maint;", 1)
	got, err := expand(edited)
	if err != nil {
		t.Fatal(err)
	}
	if got != clean {
		t.Errorf("edited block not restored:\n%s", got)
	}
}

func TestExpandViewOnly(t *testing.T) {
	t.Parallel()

	got, err := expand("DROP VIEW active_notes;\nALTER TABLE notes ADD COLUMN body text;\n-- +koala soft-delete-view v1 notes\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "-- +koala soft-delete-view v1 notes\n" + beginMarker + "\n" +
		"CREATE VIEW active_notes AS\n    SELECT * FROM notes WHERE deleted_at IS NULL;\n" +
		"GRANT SELECT, INSERT, UPDATE ON active_notes TO koala_app;\n" + endMarker + "\n"
	if !strings.HasSuffix(got, want) || strings.Contains(got, "ALTER TABLE notes\n") {
		t.Errorf("view-only block:\n%s", got)
	}
}

func TestExpandRejects(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ src, want string }{
		"unknown directive":  {"-- +koala soft-delete-all v1 notes", "unknown directive"},
		"unknown version":    {"-- +koala soft-delete v2 notes", "template version"},
		"view-like name":     {"-- +koala soft-delete v1 active_notes", "active_"},
		"long name":          {"-- +koala soft-delete v1 " + strings.Repeat("n", maxTable+1), "longer than"},
		"stray block":        {beginMarker + "\n" + endMarker, "without a directive"},
		"unterminated block": {"-- +koala soft-delete v1 notes\n" + beginMarker + "\nSELECT 1;", "has no"},
		"trailing garbage":   {"-- +koala soft-delete v1 notes please", "unknown directive"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := expand(tt.src); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestWriteRule(t *testing.T) {
	t.Parallel()

	src := "rules:\n  - name: other\n    rule: 'true'\n  - name: no-base-table\n    message: m\n    rule: 'false'\n"
	got, err := writeRule(src, `query.sql.matches(r"it's")`)
	if err != nil {
		t.Fatal(err)
	}
	want := "rules:\n  - name: other\n    rule: 'true'\n  - name: no-base-table\n    message: m\n    rule: 'query.sql.matches(r\"it''s\")'\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if _, err := writeRule("rules:\n  - name: other\n    rule: 'true'\n", "x"); err == nil {
		t.Error("missing rule: no error")
	}
}

// The committed migrations are expanded: the generator would not change
// them (gen-check proves the same in CI with the whole generate step).
func TestCommittedMigrationsAreExpanded(t *testing.T) {
	t.Parallel()

	for _, dir := range migrationDirs() {
		files, err := filepath.Glob(filepath.Join("..", "..", "..", dir, "*.sql"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no migrations in %s", dir)
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			got, err := expand(string(data))
			if err != nil {
				t.Errorf("%s: %v", f, err)
				continue
			}
			if got != string(data) {
				t.Errorf("%s is not expanded: run make generate", f)
			}
		}
	}
}
