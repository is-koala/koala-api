//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
)

// insufficientPrivilege is SQLSTATE 42501.
const insufficientPrivilege = "42501"

func wantPermissionDenied(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege {
		t.Fatalf("err = %v, want SQLSTATE %s (permission denied)", err, insufficientPrivilege)
	}
}

// TestAppCannotTouchBaseTable is the ADR-0013 guarantee: a query that
// forgets the active_ view fails on its first run instead of returning
// soft-deleted rows.
func TestAppCannotTouchBaseTable(t *testing.T) {
	t.Parallel()

	app := dbtest.New(t).Open(t, db.RoleApp)
	id := uuid.Must(uuid.NewV7())

	tests := map[string]struct {
		sql  string
		args []any
	}{
		"select":              {"SELECT id FROM example_notes", nil},
		"insert":              {"INSERT INTO example_notes (id, body) VALUES ($1, 'x')", []any{id}},
		"update":              {"UPDATE example_notes SET body = 'x' WHERE id = $1", []any{id}},
		"delete":              {"DELETE FROM example_notes WHERE id = $1", []any{id}},
		"truncate":            {"TRUNCATE example_notes", nil},
		"delete through view": {"DELETE FROM active_example_notes WHERE id = $1", []any{id}},
		"ddl":                 {"ALTER TABLE example_notes ADD COLUMN x int", nil},
		"goose table":         {"SELECT version_id FROM goose_db_version", nil},
		"temporary table":     {"CREATE TEMPORARY TABLE scratch (i int)", nil},
		"create in public":    {"CREATE TABLE public.scratch (i int)", nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := app.Pool().Exec(t.Context(), tt.sql, tt.args...)
			wantPermissionDenied(t, err)
		})
	}
}

// TestSoftDeleteThroughView: the application inserts, reads and
// soft-deletes through active_example_notes; maintenance still sees the
// deleted row in the base table.
func TestSoftDeleteThroughView(t *testing.T) {
	t.Parallel()

	d := dbtest.New(t)
	app := d.Open(t, db.RoleApp)
	maint := d.Open(t, db.RoleMaint)
	ctx := t.Context()
	id, deletionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	if _, err := app.Pool().Exec(ctx, "INSERT INTO active_example_notes (id, body) VALUES ($1, 'hello')", id); err != nil {
		t.Fatalf("insert through view: %v", err)
	}
	err := app.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		if _, err := q.Exec(ctx, `INSERT INTO deletions (id, root_table, root_id, actor_kind, reason, created_at)
			VALUES ($1, 'example_notes', $2, 'system', 'retention', now())`, deletionID, id); err != nil {
			return err
		}
		_, err := q.Exec(ctx, "UPDATE active_example_notes SET deleted_at = now(), deletion_id = $1 WHERE id = $2", deletionID, id)
		return err
	})
	if err != nil {
		t.Fatalf("soft delete through view: %v", err)
	}

	var visible int
	if err := app.Pool().QueryRow(ctx, "SELECT count(*) FROM active_example_notes WHERE id = $1", id).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("soft-deleted row still in the view")
	}

	var gotDeletion uuid.UUID
	if err := maint.Pool().QueryRow(ctx, "SELECT deletion_id FROM example_notes WHERE id = $1", id).Scan(&gotDeletion); err != nil {
		t.Fatalf("maintenance reads base table: %v", err)
	}
	if gotDeletion != deletionID {
		t.Fatalf("deletion_id = %s, want %s", gotDeletion, deletionID)
	}
}

// grants are the privileges koala_app, koala_maint and PUBLIC hold on one
// object.
type grants struct {
	App, AppColumns, Maint, MaintColumns, Public []string
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

func checkGrants(t *testing.T, object string, got, want grants) {
	t.Helper()
	for _, c := range []struct {
		who       string
		got, want []string
	}{
		{"koala_app", got.App, want.App},
		{"koala_app (columns)", got.AppColumns, want.AppColumns},
		{"koala_maint", got.Maint, want.Maint},
		{"koala_maint (columns)", got.MaintColumns, want.MaintColumns},
		{"PUBLIC", got.Public, want.Public},
	} {
		if !slices.Equal(sorted(c.got), sorted(c.want)) {
			t.Errorf("%s on %s = %v, want %v", c.who, object, c.got, c.want)
		}
	}
}

// columnPart keeps the privileges that also exist per column.
func columnPart(privs []string) []string {
	var out []string
	for _, p := range privs {
		if p == "SELECT" || p == "INSERT" || p == "UPDATE" || p == "REFERENCES" {
			out = append(out, p)
		}
	}
	return out
}

// TestRoleGrants is the privilege catalog of the schema (ADR-0013,
// ADR-0017). Every object is owned by koala_owner, and koala_app,
// koala_maint and PUBLIC hold exactly what the object's class allows, on
// the database, schemas, tables, views, columns, sequences and functions.
// A grant that is missing or too wide fails here.
func TestRoleGrants(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	ctx := t.Context()
	q := owner.Pool()

	t.Run("schemas", func(t *testing.T) {
		t.Parallel()
		rows, err := q.Query(ctx, `
			SELECT n.nspname,
			       ARRAY(SELECT p FROM unnest(ARRAY['USAGE', 'CREATE']) p WHERE has_schema_privilege('koala_app', n.oid, p)),
			       ARRAY(SELECT p FROM unnest(ARRAY['USAGE', 'CREATE']) p WHERE has_schema_privilege('koala_maint', n.oid, p)),
			       ARRAY(SELECT x.privilege_type FROM aclexplode(coalesce(n.nspacl, acldefault('n', n.nspowner))) x WHERE x.grantee = 0)
			FROM pg_namespace n
			WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`)
		if err != nil {
			t.Fatal(err)
		}
		var seen []string
		for _, r := range collect[namedGrants](t, rows) {
			seen = append(seen, r.Name)
			checkGrants(t, "schema "+r.Name,
				grants{App: r.App, Maint: r.Maint, Public: r.Public},
				grants{App: []string{"USAGE"}, Maint: []string{"USAGE"}})
		}
		if !slices.Equal(seen, []string{"public"}) {
			t.Errorf("schemas = %v, want only public; a new schema needs its own grants here", seen)
		}
	})

	t.Run("database", func(t *testing.T) {
		t.Parallel()
		rows, err := q.Query(ctx, `
			SELECT d.datname,
			       ARRAY(SELECT p FROM unnest(ARRAY['CONNECT', 'TEMPORARY', 'CREATE']) p WHERE has_database_privilege('koala_app', d.oid, p)),
			       ARRAY(SELECT p FROM unnest(ARRAY['CONNECT', 'TEMPORARY', 'CREATE']) p WHERE has_database_privilege('koala_maint', d.oid, p)),
			       ARRAY(SELECT x.privilege_type FROM aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) x WHERE x.grantee = 0)
			FROM pg_database d WHERE d.datname = current_database()`)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range collect[namedGrants](t, rows) {
			checkGrants(t, "database "+r.Name,
				grants{App: r.App, Maint: r.Maint, Public: r.Public},
				grants{App: []string{"CONNECT"}, Maint: []string{"CONNECT"}})
		}
	})

	t.Run("tables and views", func(t *testing.T) {
		t.Parallel()
		rows, err := q.Query(ctx, `
			WITH privs(p) AS (VALUES ('SELECT'), ('INSERT'), ('UPDATE'), ('DELETE'), ('TRUNCATE'), ('REFERENCES'), ('TRIGGER'), ('MAINTAIN')),
			     colprivs(p) AS (VALUES ('SELECT'), ('INSERT'), ('UPDATE'), ('REFERENCES'))
			SELECT c.relname, c.relkind::text, pg_get_userbyid(c.relowner),
			       ARRAY(SELECT p FROM privs WHERE has_table_privilege('koala_app', c.oid, p)),
			       ARRAY(SELECT p FROM colprivs WHERE has_any_column_privilege('koala_app', c.oid, p)),
			       ARRAY(SELECT p FROM privs WHERE has_table_privilege('koala_maint', c.oid, p)),
			       ARRAY(SELECT p FROM colprivs WHERE has_any_column_privilege('koala_maint', c.oid, p)),
			       ARRAY(SELECT x.privilege_type FROM aclexplode(coalesce(c.relacl, acldefault('r', c.relowner))) x WHERE x.grantee = 0)
			         || ARRAY(SELECT 'column ' || a.attname || ' ' || x.privilege_type
			                  FROM pg_attribute a, aclexplode(a.attacl) x
			                  WHERE a.attrelid = c.oid AND x.grantee = 0)
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
			  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')`)
		if err != nil {
			t.Fatal(err)
		}
		dml := []string{"SELECT", "INSERT", "UPDATE", "DELETE"}
		activeView := []string{"SELECT", "INSERT", "UPDATE"} // a soft delete is an UPDATE; no DELETE
		// Base tables with an explicit decision; every other base table
		// gets nothing for koala_app.
		app := map[string][]string{
			"deletions":        {"INSERT"},           // record of deletions: insert-only, no read-back (0010_deletions.sql)
			"example_receipts": {"SELECT", "INSERT"}, // example M1 table (dbtest testdata)
			// M2 sessions, tokens and attempts: written directly and
			// purged by the application (0020, 0021, ADR-0025).
			"user_sessions":       dml,
			"staff_sessions":      dml,
			"staff_login_sources": dml,
			// Refresh tokens are deleted with their session only.
			"user_refresh_tokens":  {"SELECT", "INSERT", "UPDATE"},
			"staff_refresh_tokens": {"SELECT", "INSERT", "UPDATE"},
			// The attempt log is append-only, purged after 90 days.
			"login_attempts": {"SELECT", "INSERT", "DELETE"},
			// Staff accounts are inserted by koala_maint only (catalog app_view).
			"active_staff_members": {"SELECT", "UPDATE"},
			// Realtime tickets are inserted, redeemed by DELETE … RETURNING
			// and purged; never updated (0022).
			"realtime_tickets": {"SELECT", "INSERT", "DELETE"},
			// Staff RBAC (0040, ADR-0026): the application resolves
			// permissions from the role graph and writes nothing in it.
			"staff_roles":            {"SELECT"},
			"staff_role_permissions": {"SELECT"},
			"staff_role_includes":    {"SELECT"},
			"staff_role_assignments": {"SELECT"},
			// Evidence (0040, 0041): appended, never updated, deleted or
			// read back; the stamp triggers run as the owner.
			"authz_change_log":          {"INSERT"},
			"admin_audit_entries":       {"INSERT"},
			"admin_audit_entry_details": {"INSERT"},
			"pii_access_log":            {"INSERT"},
		}
		maint := map[string][]string{
			"deletions":        {"SELECT"}, // plus UPDATE of the restore columns only, below
			"example_receipts": {"SELECT"},
			// `koala staff` assigns roles and records the change; the
			// graph itself changes only by migration until Faz 8.
			"staff_roles":            {"SELECT"},
			"staff_role_permissions": {"SELECT"},
			"staff_role_includes":    {"SELECT"},
			"staff_role_assignments": {"SELECT", "INSERT", "DELETE"},
			"authz_change_log":       {"INSERT"},
			// `koala staff` audits as koala_maint, `koala maint
			// audit-purge` deletes past the retention (ADR-0026).
			"admin_audit_entries":       {"SELECT", "INSERT", "DELETE"},
			"admin_audit_entry_details": {"SELECT", "INSERT", "DELETE"},
			"pii_access_log":            {"SELECT", "DELETE"},
		}
		// Column-level privileges beyond the table-level ones; the exact
		// columns are checked in "column grants".
		maintColumns := map[string][]string{
			"deletions": {"SELECT", "UPDATE"},
		}
		relations := collect[relationGrants](t, rows)
		// A soft-deleted base table (one with an active_ view) is
		// koala_maint's: the generated grant of softdeletegen (ADR-0020).
		for _, r := range relations {
			if strings.HasPrefix(r.Name, "active_") && r.Kind == "v" {
				maint[strings.TrimPrefix(r.Name, "active_")] = dml
			}
		}
		seenRiverJob := false
		for _, r := range relations {
			name, kind := r.Name, r.Kind
			if r.Owner != db.RoleOwner {
				t.Errorf("%s is owned by %s, want %s", name, r.Owner, db.RoleOwner)
			}
			want := grants{Maint: maint[name]}
			switch {
			case app[name] != nil:
				want.App = app[name]
			case strings.HasPrefix(name, "river_"): // M3: River's tables (0009_river_grants.sql)
				want.App = dml
				seenRiverJob = seenRiverJob || name == "river_job"
			case strings.HasPrefix(name, "active_") && kind == "v":
				want.App = activeView
			}
			want.AppColumns = columnPart(want.App)
			want.MaintColumns = columnPart(want.Maint)
			if cols, ok := maintColumns[name]; ok {
				want.MaintColumns = cols
			}
			checkGrants(t, name, grants{
				App: r.App, AppColumns: r.AppColumns,
				Maint: r.Maint, MaintColumns: r.MaintColumns, Public: r.Public,
			}, want)
		}
		if !seenRiverJob {
			t.Error("river_job not found: River's migrations did not run")
		}
	})

	// Column-level grants exist only where a role may change some columns
	// of a table and not others.
	t.Run("column grants", func(t *testing.T) {
		t.Parallel()
		rows, err := q.Query(ctx, `
			SELECT c.relname || '.' || a.attname || ': ' ||
			       CASE WHEN x.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(x.grantee)::text END || ' ' || x.privilege_type
			FROM pg_attribute a
			JOIN pg_class c ON c.oid = a.attrelid, aclexplode(a.attacl) x
			WHERE c.relnamespace = 'public'::regnamespace AND a.attacl IS NOT NULL
			`)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got) // byte order, not the database's collation
		want := []string{
			"deletions.restored_at: koala_maint UPDATE",
			"deletions.restored_by: koala_maint UPDATE",
			"deletions.restored_by_kind: koala_maint UPDATE",
		}
		if !slices.Equal(got, want) {
			t.Errorf("column grants = %v, want %v", got, want)
		}
	})

	// No role may pass a privilege on: every ACL entry that is grantable
	// belongs to the object's owner.
	t.Run("no grant options", func(t *testing.T) {
		t.Parallel()
		rows, err := q.Query(ctx, `
			WITH ours AS (
			    SELECT oid FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname <> 'information_schema'
			),
			acl(object, owner, grantee, privilege, grantable) AS (
			    SELECT 'database ' || d.datname, d.datdba, x.grantee, x.privilege_type, x.is_grantable
			    FROM pg_database d, aclexplode(d.datacl) x WHERE d.datname = current_database()
			  UNION ALL
			    SELECT 'schema ' || n.nspname, n.nspowner, x.grantee, x.privilege_type, x.is_grantable
			    FROM pg_namespace n, aclexplode(n.nspacl) x WHERE n.oid IN (SELECT oid FROM ours)
			  UNION ALL
			    SELECT 'relation ' || c.relname, c.relowner, x.grantee, x.privilege_type, x.is_grantable
			    FROM pg_class c, aclexplode(c.relacl) x WHERE c.relnamespace IN (SELECT oid FROM ours)
			  UNION ALL
			    SELECT 'column ' || c.relname || '.' || a.attname, c.relowner, x.grantee, x.privilege_type, x.is_grantable
			    FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid, aclexplode(a.attacl) x
			    WHERE c.relnamespace IN (SELECT oid FROM ours)
			  UNION ALL
			    SELECT 'function ' || p.proname, p.proowner, x.grantee, x.privilege_type, x.is_grantable
			    FROM pg_proc p, aclexplode(p.proacl) x WHERE p.pronamespace IN (SELECT oid FROM ours)
			)
			SELECT object || ': ' || CASE WHEN grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(grantee)::text END
			       || ' ' || privilege
			FROM acl
			WHERE grantable AND grantee <> owner
			ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range lines {
			t.Errorf("grantable privilege: %s", line)
		}
	})

	t.Run("sequences", func(t *testing.T) {
		t.Parallel()
		rows, err := q.Query(ctx, `
			WITH privs(p) AS (VALUES ('USAGE'), ('SELECT'), ('UPDATE'))
			SELECT c.relname, pg_get_userbyid(c.relowner),
			       ARRAY(SELECT p FROM privs WHERE has_sequence_privilege('koala_app', c.oid, p)),
			       ARRAY(SELECT p FROM privs WHERE has_sequence_privilege('koala_maint', c.oid, p)),
			       ARRAY(SELECT x.privilege_type FROM aclexplode(coalesce(c.relacl, acldefault('s', c.relowner))) x WHERE x.grantee = 0)
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND c.relkind = 'S'`)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range collect[ownedGrants](t, rows) {
			if r.Owner != db.RoleOwner {
				t.Errorf("sequence %s is owned by %s", r.Name, r.Owner)
			}
			var want grants
			if strings.HasPrefix(r.Name, "river_") {
				want.App = []string{"USAGE", "SELECT"}
			}
			checkGrants(t, "sequence "+r.Name, grants{App: r.App, Maint: r.Maint, Public: r.Public}, want)
		}
	})

	t.Run("functions", func(t *testing.T) {
		t.Parallel()
		rows, err := q.Query(ctx, `
			SELECT p.proname, pg_get_userbyid(p.proowner),
			       ARRAY(SELECT 'EXECUTE' WHERE has_function_privilege('koala_app', p.oid, 'EXECUTE')),
			       ARRAY(SELECT 'EXECUTE' WHERE has_function_privilege('koala_maint', p.oid, 'EXECUTE')),
			       ARRAY(SELECT x.privilege_type FROM aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) x WHERE x.grantee = 0)
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, r := range collect[ownedGrants](t, rows) {
			seen++
			if r.Owner != db.RoleOwner {
				t.Errorf("function %s is owned by %s", r.Name, r.Owner)
			}
			var want grants
			if strings.HasPrefix(r.Name, "river_") {
				want.App = []string{"EXECUTE"}
			}
			checkGrants(t, "function "+r.Name, grants{App: r.App, Maint: r.Maint, Public: r.Public}, want)
		}
		if seen == 0 {
			t.Error("no functions found: River's river_job_state_in_bitmask is missing")
		}
	})
}

// Row shapes of the catalog queries, scanned by column position.
type (
	namedGrants struct {
		Name               string
		App, Maint, Public []string
	}
	ownedGrants struct {
		Name, Owner        string
		App, Maint, Public []string
	}
	relationGrants struct {
		Name, Kind, Owner                            string
		App, AppColumns, Maint, MaintColumns, Public []string
	}
)

func collect[T any](t *testing.T, rows pgx.Rows) []T {
	t.Helper()
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[T])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestConcurrentIndexIsValid: the NO TRANSACTION migration ran and left a
// valid index.
func TestConcurrentIndexIsValid(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	var valid bool
	err := owner.Pool().QueryRow(t.Context(),
		"SELECT indisvalid FROM pg_index WHERE indexrelid = 'example_notes_active_created_at_idx'::regclass").Scan(&valid)
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatal("index is INVALID")
	}
}
