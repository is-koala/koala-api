//go:build integration

package app

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/db/migrations"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
)

// statusCounts counts the "applied" and "pending" rows of migrate status.
func statusCounts(t *testing.T, stdout string) (applied, pending int) {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		switch strings.SplitN(line, " ", 2)[0] {
		case "applied":
			applied++
		case "pending":
			pending++
		}
	}
	return applied, pending
}

// TestMigrateCommand drives `koala migrate` on an empty database as a
// login that is a member of koala_owner: status, up, an idempotent second
// up, down through every migration back to an empty schema, and up again
// (the down scripts leave nothing behind that blocks a re-run).
func TestMigrateCommand(t *testing.T) {
	t.Parallel()

	d := dbtest.NewEmpty(t)
	environ := []string{"KOALA_ENV=test", "KOALA_OWNER_DATABASE_URL=" + d.URL(db.RoleOwner)}
	owner := d.Open(t, db.RoleOwner)

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	total := len(entries)

	migrate := func(sub string, wantCode int) string {
		t.Helper()
		code, stdout, stderr := run([]string{"migrate", sub}, environ...)
		if code != wantCode {
			t.Fatalf("migrate %s: exit %d, want %d\nstdout: %s\nstderr: %s", sub, code, wantCode, stdout, stderr)
		}
		return stdout
	}
	wantStatus := func(wantApplied int) {
		t.Helper()
		applied, pending := statusCounts(t, migrate("status", ExitOK))
		if applied != wantApplied || pending != total-wantApplied {
			t.Fatalf("status: %d applied, %d pending; want %d and %d", applied, pending, wantApplied, total-wantApplied)
		}
	}
	riverTables := func() int {
		t.Helper()
		var n int
		err := owner.Pool().QueryRow(t.Context(),
			"SELECT count(*) FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relname LIKE 'river%'").Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	wantStatus(0)
	migrate("up", ExitOK)
	wantStatus(total)
	if riverTables() == 0 {
		t.Fatal("no River tables after up")
	}
	migrate("up", ExitOK) // nothing pending: still success

	for range total {
		migrate("down", ExitOK)
	}
	wantStatus(0)
	if n := riverTables(); n != 0 {
		t.Fatalf("%d River relations left after migrating down", n)
	}
	migrate("down", ExitError) // nothing left to roll back

	migrate("up", ExitOK)
	wantStatus(total)
}

// TestMigrateRefusesNonOwner: a login that is not a member of koala_owner
// cannot SET ROLE koala_owner, so it cannot migrate, and says why.
func TestMigrateRefusesNonOwner(t *testing.T) {
	t.Parallel()

	d := dbtest.NewEmpty(t)
	code, stdout, _ := run([]string{"migrate", "up"}, "KOALA_ENV=test", "KOALA_OWNER_DATABASE_URL="+d.URL(db.RoleApp))
	if code != ExitError {
		t.Fatalf("exit = %d, want %d; stdout: %s", code, ExitError, stdout)
	}
	for _, want := range []string{"set role koala_owner", "permission denied to set role", "42501"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout does not explain the refusal (missing %q): %s", want, stdout)
		}
	}
}

// TestBootstrapIsIdempotent: koala bootstrap runs in every release with a
// CREATEROLE login (not a superuser) and succeeds when the roles exist.
func TestBootstrapIsIdempotent(t *testing.T) {
	t.Parallel()

	// dbtest creates the roles first, under its lock, so this test never
	// races another package's harness on CREATE ROLE.
	d := dbtest.New(t)
	login := d.NewLogin(t, dbtest.LoginOptions{Attributes: []string{"CREATEROLE"}})
	environ := []string{"KOALA_ENV=test", "KOALA_BOOTSTRAP_DATABASE_URL=" + login.URLFor("postgres")}
	for range 2 {
		if code, stdout, stderr := run([]string{"bootstrap"}, environ...); code != ExitOK {
			t.Fatalf("bootstrap: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
	}
}

// TestOwnerCommandsRefuseSuperuser: migrate, maint and bootstrap refuse a
// superuser login (ADR-0017).
func TestOwnerCommandsRefuseSuperuser(t *testing.T) {
	t.Parallel()

	d := dbtest.NewEmpty(t)
	super := d.NewLogin(t, dbtest.LoginOptions{Attributes: []string{"SUPERUSER"}})
	tests := []struct {
		args    []string
		environ string
	}{
		{[]string{"migrate", "status"}, "KOALA_OWNER_DATABASE_URL=" + super.URL},
		{[]string{"maint", "reindex"}, "KOALA_OWNER_DATABASE_URL=" + super.URL},
		{[]string{"bootstrap"}, "KOALA_BOOTSTRAP_DATABASE_URL=" + super.URLFor("postgres")},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			code, stdout, _ := run(tt.args, "KOALA_ENV=test", tt.environ)
			if code != ExitError || !strings.Contains(stdout, "superuser login refused") {
				t.Fatalf("exit %d, stdout %s; want a superuser refusal", code, stdout)
			}
		})
	}
}

// TestServeRefusesPrivilegedLogin: serve does not start with a login that
// could leave koala_app (ADR-0017).
func TestServeRefusesPrivilegedLogin(t *testing.T) {
	t.Parallel()

	d := dbtest.New(t)
	login := d.NewLogin(t, dbtest.LoginOptions{Roles: []string{db.RoleApp, db.RoleMaint}})
	// The check runs before the listener opens; port 1 is never bound.
	code, stdout, _ := run([]string{"serve"}, "KOALA_ENV=test", "KOALA_HTTP_ADDR=127.0.0.1:1", "KOALA_DATABASE_URL="+login.URL)
	if code != ExitError || !strings.Contains(stdout, "more privileged than koala_app") {
		t.Fatalf("exit %d, stdout %s; want a refusal naming the privilege problem", code, stdout)
	}
}

// TestMaintReindex: koala maint reindex drops an invalid leftover of an
// interrupted concurrent rebuild of a River index and rebuilds River's
// indexes. A valid index with a leftover-like name and an invalid index
// under another name are not River's leftovers and stay.
func TestMaintReindex(t *testing.T) {
	t.Parallel()

	d := dbtest.New(t)
	owner := d.Open(t, db.RoleOwner)
	ctx := t.Context()
	exec := func(sql string) error {
		t.Helper()
		_, err := owner.Pool().Exec(ctx, sql)
		return err
	}

	if err := exec("INSERT INTO river_job (kind, args, max_attempts) VALUES ('dup', '{}', 1), ('dup', '{}', 1)"); err != nil {
		t.Fatal(err)
	}
	// A failing CREATE UNIQUE INDEX CONCURRENTLY leaves an INVALID index
	// behind, exactly like an interrupted REINDEX CONCURRENTLY does.
	for _, name := range []string{"river_job_kind_ccnew", "example_invalid_kind_idx"} {
		if err := exec("CREATE UNIQUE INDEX CONCURRENTLY " + name + " ON river_job (kind)"); err == nil {
			t.Fatalf("setup: %s should have failed on the duplicate rows", name)
		}
	}
	if err := exec("CREATE INDEX river_job_args_index_ccnew ON river_job (kind)"); err != nil {
		t.Fatal(err)
	}

	indexes := func() map[string]bool {
		t.Helper()
		rows, err := owner.Pool().Query(ctx, `
			SELECT c.relname::text, i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			WHERE i.indrelid = 'river_job'::regclass`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]bool{}
		for rows.Next() {
			var name string
			var valid bool
			if err := rows.Scan(&name, &valid); err != nil {
				t.Fatal(err)
			}
			got[name] = valid
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if valid, ok := indexes()["river_job_kind_ccnew"]; !ok || valid {
		t.Fatal("setup: no invalid leftover index")
	}

	code, stdout, stderr := run([]string{"maint", "reindex"}, "KOALA_ENV=test", "KOALA_OWNER_DATABASE_URL="+d.URL(db.RoleOwner))
	if code != ExitOK {
		t.Fatalf("maint reindex: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"index":"river_job_prioritized_fetching_index"`) {
		t.Fatalf("no reindex logged: %s", stdout)
	}

	after := indexes()
	if _, ok := after["river_job_kind_ccnew"]; ok {
		t.Error("River's invalid leftover river_job_kind_ccnew is still there")
	}
	if valid, ok := after["river_job_args_index_ccnew"]; !ok || !valid {
		t.Error("the valid index river_job_args_index_ccnew was dropped or invalidated")
	}
	if valid, ok := after["example_invalid_kind_idx"]; !ok || valid {
		t.Error("the invalid index under another name was touched")
	}
	for _, name := range []string{
		"river_job_args_index", "river_job_kind", "river_job_metadata_index", "river_job_pkey",
		"river_job_prioritized_fetching_index", "river_job_state_and_finalized_at_index", "river_job_unique_idx",
	} {
		if valid, ok := after[name]; !ok || !valid {
			t.Errorf("River index %s missing or invalid after reindex", name)
		}
	}
}
