//go:build integration

package softdelete_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// TestCatalogMatchesDatabase holds db/softdelete.yaml (merged with the
// example tables' catalog) equal to the migrated schema (ADR-0013, plan
// §6.1 Faz 1 çıkış kriterleri). A new table, view or foreign key fails here
// until the catalog classifies it.
func TestCatalogMatchesDatabase(t *testing.T) {
	t.Parallel()

	q := dbtest.New(t).Open(t, db.RoleOwner).Pool()
	cat := dbtest.SoftDeleteCatalog(t)

	t.Run("every table is soft-deleted or exempt", func(t *testing.T) {
		t.Parallel()
		inDB := queryStrings(t, q, `
			SELECT c.relname::text FROM pg_class c
			WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'p')
			ORDER BY 1`)
		var inCatalog []string
		for _, tb := range cat.Tables() {
			inCatalog = append(inCatalog, tb.Name)
		}
		for _, name := range inDB {
			if !slices.Contains(inCatalog, name) {
				t.Errorf("table %s is not in db/softdelete.yaml: classify it as soft_delete or as an exemption (M1–M6) with a reason", name)
			}
		}
		for _, name := range inCatalog {
			if !slices.Contains(inDB, name) {
				t.Errorf("db/softdelete.yaml lists %s, which the database does not have", name)
			}
		}
	})

	t.Run("every view is the active view of a soft-deleted table", func(t *testing.T) {
		t.Parallel()
		views := queryStrings(t, q, `
			SELECT c.relname::text FROM pg_class c
			WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('v', 'm')
			ORDER BY 1`)
		want := []string{}
		for _, name := range cat.SoftDeleteTables() {
			tb, _ := cat.Table(name)
			want = append(want, tb.View())
		}
		slices.Sort(want)
		if !slices.Equal(views, want) {
			t.Errorf("views = %v, want the active views %v", views, want)
		}
	})

	for _, tb := range cat.Tables() {
		if tb.SoftDelete() {
			t.Run("soft-deleted "+tb.Name, func(t *testing.T) {
				t.Parallel()
				checkSoftDeleted(t, q, tb)
			})
			continue
		}
		t.Run("exempt "+tb.Name, func(t *testing.T) {
			t.Parallel()
			checkExempt(t, q, tb)
		})
	}

	t.Run("every foreign key to a soft-deleted table has a policy", func(t *testing.T) {
		t.Parallel()
		checkEdges(t, q, cat)
	})
}

type column struct{ Name, Type string }

func columns(t *testing.T, q *pgxpool.Pool, relation string) []column {
	t.Helper()
	rows, err := q.Query(t.Context(), `
		SELECT attname::text, format_type(atttypid, atttypmod)
		FROM pg_attribute
		WHERE attrelid = to_regclass($1) AND attnum > 0 AND NOT attisdropped
		ORDER BY attnum`, relation)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[column])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func checkSoftDeleted(t *testing.T, q *pgxpool.Pool, tb softdelete.Table) {
	t.Helper()
	ctx := t.Context()
	cols := columns(t, q, tb.Name)

	for _, want := range []column{{"id", "uuid"}, {"deleted_at", "timestamp with time zone"}, {"deletion_id", "uuid"}} {
		if !slices.Contains(cols, want) {
			t.Errorf("%s has no column %s %s", tb.Name, want.Name, want.Type)
		}
	}

	// The primary key is id: Delete and Restore address rows by it.
	pk := queryStrings(t, q, `
		SELECT a.attname::text FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
		WHERE i.indrelid = to_regclass($1) AND i.indisprimary`, tb.Name)
	if !slices.Equal(pk, []string{"id"}) {
		t.Errorf("%s primary key = %v, want (id)", tb.Name, pk)
	}

	// deletion_id references deletions; the CHECK keeps the two columns
	// together.
	var fkOK, checkOK bool
	err := q.QueryRow(ctx, `
		SELECT
		  EXISTS (SELECT FROM pg_constraint c
		          JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		          WHERE c.conrelid = to_regclass($1) AND c.contype = 'f'
		            AND cardinality(c.conkey) = 1 AND a.attname = 'deletion_id'
		            AND c.confrelid = 'deletions'::regclass),
		  EXISTS (SELECT FROM pg_constraint c
		          WHERE c.conrelid = to_regclass($1) AND c.contype = 'c'
		            AND pg_get_constraintdef(c.oid) LIKE '%(deleted_at IS NULL) = (deletion_id IS NULL)%')`,
		tb.Name).Scan(&fkOK, &checkOK)
	if err != nil {
		t.Fatal(err)
	}
	if !fkOK {
		t.Errorf("%s.deletion_id does not reference deletions", tb.Name)
	}
	if !checkOK {
		t.Errorf("%s has no CHECK ((deleted_at IS NULL) = (deletion_id IS NULL))", tb.Name)
	}

	// The view: same columns as the table, filtered on deleted_at, no
	// CHECK OPTION, a security barrier exactly when the table holds PII.
	if viewCols := columns(t, q, tb.View()); !slices.Equal(viewCols, cols) {
		t.Errorf("%s columns %v, table columns %v: recreate the view after changing the table (-- +koala soft-delete-view)", tb.View(), viewCols, cols)
	}
	var def string
	var options []string
	err = q.QueryRow(ctx, `
		SELECT pg_get_viewdef(c.oid), coalesce(c.reloptions, '{}')
		FROM pg_class c WHERE c.oid = to_regclass($1) AND c.relkind = 'v'`, tb.View()).Scan(&def, &options)
	if err != nil {
		t.Fatalf("view %s: %v", tb.View(), err)
	}
	if !strings.Contains(def, "FROM "+tb.Name) || !strings.Contains(def, "WHERE (deleted_at IS NULL)") {
		t.Errorf("%s is not `SELECT * FROM %s WHERE deleted_at IS NULL`: %s", tb.View(), tb.Name, def)
	}
	barrier := false
	for _, o := range options {
		switch {
		case o == "security_barrier=true":
			barrier = true
		case strings.HasPrefix(o, "check_option"):
			t.Errorf("%s has %s: the soft delete moves a row out of the view", tb.View(), o)
		}
	}
	if barrier != tb.PII {
		t.Errorf("%s security_barrier = %v, catalog pii = %v (KVKK, ADR-0017 §8)", tb.View(), barrier, tb.PII)
	}

	// koala_app works on the view only; koala_maint on the table.
	// (Column privileges: TestRoleGrants.)
	var appOnTable, appOnView, maintOnTable []string
	err = q.QueryRow(ctx, `
		WITH privs(p) AS (VALUES ('SELECT'), ('INSERT'), ('UPDATE'), ('DELETE'), ('TRUNCATE'), ('REFERENCES'), ('TRIGGER'), ('MAINTAIN'))
		SELECT ARRAY(SELECT p FROM privs WHERE has_table_privilege('koala_app', to_regclass($1), p)),
		       ARRAY(SELECT p FROM privs WHERE has_table_privilege('koala_app', to_regclass($2), p)),
		       ARRAY(SELECT p FROM privs WHERE has_table_privilege('koala_maint', to_regclass($1), p))`,
		tb.Name, tb.View()).Scan(&appOnTable, &appOnView, &maintOnTable)
	if err != nil {
		t.Fatal(err)
	}
	if len(appOnTable) > 0 {
		t.Errorf("koala_app has %v on the base table %s", appOnTable, tb.Name)
	}
	if want := tb.AppViewPrivileges(); !slices.Equal(appOnView, want) {
		t.Errorf("koala_app on %s = %v, want %v (catalog app_view)", tb.View(), appOnView, want)
	}
	if !slices.Equal(maintOnTable, []string{"SELECT", "INSERT", "UPDATE", "DELETE"}) {
		t.Errorf("koala_maint on %s = %v, want SELECT, INSERT, UPDATE, DELETE", tb.Name, maintOnTable)
	}

	// Business keys are unique among active rows only.
	rows, err := q.Query(ctx, `
		SELECT ic.relname::text, coalesce(pg_get_expr(i.indpred, i.indrelid), '')
		FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
		WHERE i.indrelid = to_regclass($1) AND i.indisunique AND NOT i.indisprimary`, tb.Name)
	if err != nil {
		t.Fatal(err)
	}
	type index struct{ Name, Predicate string }
	indexes, err := pgx.CollectRows(rows, pgx.RowToStructByPos[index])
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ix := range indexes {
		names = append(names, ix.Name)
		if _, excepted := tb.UniqueExceptions[ix.Name]; excepted {
			continue
		}
		if !strings.Contains(ix.Predicate, "deleted_at IS NULL") {
			t.Errorf("unique index %s on %s is not partial on deleted_at IS NULL; a deleted row would block the key (or add a unique_exceptions entry)", ix.Name, tb.Name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(tb.UniqueExceptions)) {
		if !slices.Contains(names, name) {
			t.Errorf("unique_exceptions of %s lists %s, which is not a unique index of it", tb.Name, name)
		}
	}
}

func checkExempt(t *testing.T, q *pgxpool.Pool, tb softdelete.Table) {
	t.Helper()
	for _, c := range columns(t, q, tb.Name) {
		if c.Name == "deleted_at" || c.Name == "deletion_id" {
			t.Errorf("exempt table %s (%s) has %s; soft-deleted tables are class soft_delete", tb.Name, tb.Class, c.Name)
		}
	}
	if tb.Class == softdelete.ClassTool {
		// A tool's table: no application role holds anything on it,
		// table- or column-level.
		var privs []string
		err := q.QueryRow(t.Context(), `
			WITH privs(p) AS (VALUES ('SELECT'), ('INSERT'), ('UPDATE'), ('DELETE'), ('TRUNCATE'), ('REFERENCES'), ('TRIGGER'), ('MAINTAIN')),
			     colprivs(p) AS (VALUES ('SELECT'), ('INSERT'), ('UPDATE'), ('REFERENCES'))
			SELECT ARRAY(SELECT r || ' ' || p FROM unnest(ARRAY['koala_app', 'koala_maint']) r, privs
			             WHERE has_table_privilege(r, to_regclass($1), p))
			    || ARRAY(SELECT r || ' column ' || p FROM unnest(ARRAY['koala_app', 'koala_maint']) r, colprivs
			             WHERE has_any_column_privilege(r, to_regclass($1), p))`, tb.Name).Scan(&privs)
		if err != nil {
			t.Fatal(err)
		}
		if len(privs) > 0 {
			t.Errorf("tool table %s grants %v; tool tables grant nothing to application roles", tb.Name, privs)
		}
		return
	}
	if tb.Class != softdelete.ClassLedger {
		return
	}
	// M1 is never deleted: no role but the owner may DELETE or TRUNCATE.
	for _, role := range []string{db.RoleApp, db.RoleMaint} {
		var can bool
		err := q.QueryRow(t.Context(),
			"SELECT has_table_privilege($1, to_regclass($2), 'DELETE') OR has_table_privilege($1, to_regclass($2), 'TRUNCATE')",
			role, tb.Name).Scan(&can)
		if err != nil {
			t.Fatal(err)
		}
		if can {
			t.Errorf("%s can delete from the M1 table %s", role, tb.Name)
		}
	}
}

type foreignKey struct {
	Child, Column, Parent, Referenced string
	Columns                           int
}

func checkEdges(t *testing.T, q *pgxpool.Pool, cat *softdelete.Catalog) {
	t.Helper()
	rows, err := q.Query(t.Context(), `
		SELECT cl.relname::text, a.attname::text, pl.relname::text, ra.attname::text, cardinality(c.conkey)
		FROM pg_constraint c
		JOIN pg_class cl ON cl.oid = c.conrelid
		JOIN pg_class pl ON pl.oid = c.confrelid
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		JOIN pg_attribute ra ON ra.attrelid = c.confrelid AND ra.attnum = c.confkey[1]
		WHERE c.contype = 'f' AND cl.relnamespace = 'public'::regnamespace
		ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	fks, err := pgx.CollectRows(rows, pgx.RowToStructByPos[foreignKey])
	if err != nil {
		t.Fatal(err)
	}
	policies := map[string]softdelete.Edge{}
	for _, e := range cat.Edges() {
		policies[e.Child+"."+e.Column] = e
	}
	seen := map[string]bool{}
	for _, fk := range fks {
		parent, ok := cat.Table(fk.Parent)
		if !ok || !parent.SoftDelete() {
			continue
		}
		key := fk.Child + "." + fk.Column
		seen[key] = true
		e, ok := policies[key]
		switch {
		case fk.Columns != 1 || fk.Referenced != "id":
			t.Errorf("foreign key %s -> %s must be one column referencing id", key, fk.Parent)
		case !ok:
			t.Errorf("foreign key %s -> %s has no policy in db/softdelete.yaml (cascade, restrict or detach)", key, fk.Parent)
		case e.Parent != fk.Parent:
			t.Errorf("edge %s names parent %s, the foreign key references %s", key, e.Parent, fk.Parent)
		case e.Policy != softdelete.Detach && !leadingIndex(t, q, fk.Child, fk.Column):
			// Delete, Restore and Orphans look children up by this
			// column; without an index each lookup scans the table.
			t.Errorf("%s edge %s has no index whose first column is %s", e.Policy, key, fk.Column)
		}
	}
	for key := range policies {
		if !seen[key] {
			t.Errorf("db/softdelete.yaml has edge %s, which is no foreign key to a soft-deleted table", key)
		}
	}
}

func queryStrings(t *testing.T, q *pgxpool.Pool, sql string, args ...any) []string {
	t.Helper()
	rows, err := q.Query(t.Context(), sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// leadingIndex reports whether table has a valid index whose first column
// is column.
func leadingIndex(t *testing.T, q *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var ok bool
	err := q.QueryRow(t.Context(), `
		SELECT EXISTS (SELECT FROM pg_index i
		               JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
		               WHERE i.indrelid = to_regclass($1) AND a.attname = $2 AND i.indisvalid)`,
		table, column).Scan(&ok)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}
