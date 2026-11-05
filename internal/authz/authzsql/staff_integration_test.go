//go:build integration

package authzsql_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/authzsql"
	"github.com/is-koala/koala-api/internal/authz/internal/matrix"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
)

// fixtureNamespace is the matrix runners' namespace (internal/authz
// fixture_test.go): staff:<name> is the same UUID in all three runners.
var fixtureNamespace = uuid.MustParse("6f1b0d7e-3c8a-4f5e-9a51-2b7c0e4d8a10")

func fixtureID(name string) uuid.UUID { return uuid.NewSHA1(fixtureNamespace, []byte(name)) }

// staffDB is a database seeded with the matrix's staff fixtures: the
// migration's roles, grants and inclusions as they are, and one staff
// member per assignment tuple of the matrix.
type staffDB struct {
	app, owner, maint *db.DB
	// roleID maps a role code in lower case (the matrix id) to its id.
	roleID map[string]uuid.UUID
	mx     *matrix.Matrix
}

func newStaffDB(t *testing.T) *staffDB {
	t.Helper()
	d := dbtest.New(t)
	s := &staffDB{app: d.Open(t, db.RoleApp), owner: d.Open(t, db.RoleOwner), maint: d.Open(t, db.RoleMaint), roleID: map[string]uuid.UUID{}}
	mx, err := matrix.Load("../testdata/matrix.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s.mx = mx
	rows, err := s.owner.Pool().Query(t.Context(), "SELECT lower(code), id FROM staff_roles")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		var id uuid.UUID
		if err := rows.Scan(&code, &id); err != nil {
			t.Fatal(err)
		}
		s.roleID[code] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	for name := range mx.Fixtures.Subjects {
		staff, ok := strings.CutPrefix(name, "staff:")
		if !ok {
			continue
		}
		_, err := s.maint.Pool().Exec(t.Context(), `
			INSERT INTO staff_members (id, email, password_hash, status, must_change_password, created_at)
			VALUES ($1, $2, '$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaA', 'ACTIVE', false, $3)`,
			fixtureID(staff), staff+"@iskoala.com", now)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, tp := range mx.Tuples() {
		obj, rest, _ := strings.Cut(tp, "#")
		rel, user, _ := strings.Cut(rest, "@")
		role, isRole := strings.CutPrefix(obj, "staff_role:")
		staff, isStaff := strings.CutPrefix(user, "staff:")
		if !isRole || !isStaff || rel != "assignee" {
			continue
		}
		_, err := s.maint.Pool().Exec(t.Context(),
			"INSERT INTO staff_role_assignments (staff_id, role_id, created_at) VALUES ($1, $2, $3)",
			fixtureID(staff), s.roleID[role], now)
		if err != nil {
			t.Fatalf("assign %s to %s: %v", role, staff, err)
		}
	}
	return s
}

// symbolic maps a database tuple's ids back to the matrix's names.
func (s *staffDB) symbolic(ref string) string {
	typ, id, _ := strings.Cut(ref, ":")
	switch typ {
	case authz.TypeStaffRole:
		for code, rid := range s.roleID {
			if rid.String() == id {
				return typ + ":" + code
			}
		}
	case authz.TypeStaff:
		for name := range s.mx.Fixtures.Subjects {
			if staff, ok := strings.CutPrefix(name, "staff:"); ok && fixtureID(staff).String() == id {
				return name
			}
		}
	}
	return ref
}

// object maps a matrix object to the database's.
func (s *staffDB) object(t *testing.T, ref string) authz.Object {
	t.Helper()
	if code, ok := strings.CutPrefix(ref, "staff_role:"); ok {
		return authz.Object{Type: authz.TypeStaffRole, ID: s.roleID[code].String()}
	}
	o, ok := authz.ParseObject(ref)
	if !ok {
		t.Fatalf("object %q", ref)
	}
	return o
}

func subject(t *testing.T, ref string) authz.Subject {
	t.Helper()
	if name, ok := strings.CutPrefix(ref, "staff:"); ok {
		return authz.Staff(fixtureID(name))
	}
	if name, ok := strings.CutPrefix(ref, "user:"); ok {
		return authz.User(fixtureID(name))
	}
	t.Fatalf("subject %q", ref)
	return authz.Subject{}
}

func staffCase(c *matrix.Case) bool {
	return c.Engine == matrix.EngineBoth && (strings.HasPrefix(c.Action, authz.TypePlatform+".") || strings.HasPrefix(c.Action, authz.TypeStaffRole+"."))
}

// TestStaffMatrixOnTheDatabase is the database leg of the conformance
// runner (plan §5.6-5, ADR-0026). The migration's role graph, with the
// matrix's staff assigned, projects through authz_tuples to exactly the
// matrix's staff tuples, the ones OpenFGA is given in the conformance
// job; and every staff row of the matrix decides on the database, through
// the one recursive query and the evaluator, as the matrix expects, with
// a basis that names the role.
func TestStaffMatrixOnTheDatabase(t *testing.T) {
	t.Parallel()
	s := newStaffDB(t)
	ctx := t.Context()

	t.Run("authz_tuples equals the matrix's staff tuples", func(t *testing.T) {
		t.Parallel()
		rows, err := s.owner.Pool().Query(ctx, `
			SELECT object_type || ':' || object_id || '#' || relation || '@' || user_type || ':' || user_id
			       || coalesce('#' || user_relation, '')
			FROM authz_tuples`)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, tp := range raw {
			obj, rest, _ := strings.Cut(tp, "#")
			rel, user, _ := strings.Cut(rest, "@")
			userRef, userRel, hasRel := strings.Cut(user, "#")
			line := s.symbolic(obj) + "#" + rel + "@" + s.symbolic(userRef)
			if hasRel {
				line += "#" + userRel
			}
			got = append(got, line)
		}
		var want []string
		for _, tp := range s.mx.Tuples() {
			if strings.HasPrefix(tp, "staff_role:") || strings.HasPrefix(tp, "platform:") {
				want = append(want, tp)
			}
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("authz_tuples:\n%v\nmatrix staff tuples:\n%v", got, want)
		}
	})

	t.Run("every staff row decides on the database as the matrix expects", func(t *testing.T) {
		t.Parallel()
		model, err := authz.DefaultModel()
		if err != nil {
			t.Fatal(err)
		}
		checker := authzsql.NewStaffChecker(model, s.app.Pool())
		ran := 0
		for i := range s.mx.Cases {
			c := &s.mx.Cases[i]
			if !staffCase(c) {
				continue
			}
			ran++
			_, relation, _ := s.mx.Relation(c.Action)
			d, err := checker.Check(ctx, subject(t, c.Subject), authz.Action(relation), s.object(t, c.Object))
			if err != nil {
				t.Fatalf("%s: %v", c.ID, err)
			}
			switch {
			case d.Allowed != c.Allowed():
				t.Errorf("%s %s %s on %s: allowed %v, want %s", c.ID, c.Subject, relation, c.Object, d.Allowed, c.Expect)
			case d.Allowed && !strings.Contains(d.Basis.String(), "staff_role:"):
				t.Errorf("%s: basis %q names no staff_role", c.ID, d.Basis)
			case !d.Allowed && d.Conceal != *c.Conceal:
				t.Errorf("%s: conceal %v, want %v", c.ID, d.Conceal, *c.Conceal)
			}
		}
		if ran < 90 {
			t.Fatalf("%d staff rows ran, want the matrix's 98", ran)
		}
	})
}

// AUTH-012: permissions are read from the role graph on every check, so
// revoking a membership counts on the very next decision, and so does
// removing an inclusion. Nothing is cached between checks.
func TestRevocationCountsOnTheNextCheck(t *testing.T) {
	t.Parallel()
	s := newStaffDB(t)
	ctx := t.Context()
	model, err := authz.DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	checker := authzsql.NewStaffChecker(model, s.app.Pool())
	allowed := func(staff string, a authz.Action) bool {
		t.Helper()
		d, err := checker.Check(ctx, authz.Staff(fixtureID(staff)), a, authz.Platform())
		if err != nil {
			t.Fatal(err)
		}
		return d.Allowed
	}
	if !allowed("s_finance_officer", authz.ActionPlatformPayoutReconcile) {
		t.Fatal("AUTH-012: FINANCE_OFFICER does not hold payout.reconcile")
	}
	_, err = s.maint.Pool().Exec(ctx, "DELETE FROM staff_role_assignments WHERE staff_id = $1", fixtureID("s_finance_officer"))
	if err != nil {
		t.Fatal(err)
	}
	if allowed("s_finance_officer", authz.ActionPlatformPayoutReconcile) {
		t.Error("AUTH-012: payout.reconcile still held after the role was revoked")
	}

	// TRUST_LEAD reaches user.read through MODERATOR → SUPPORT_AGENT and
	// DISPUTE_HANDLER → SUPPORT_AGENT; cutting both edges below
	// TRUST_LEAD ends it.
	if !allowed("s_trust_lead", authz.ActionPlatformUserRead) {
		t.Fatal("TRUST_LEAD does not reach user.read")
	}
	_, err = s.owner.Pool().Exec(ctx, "DELETE FROM staff_role_includes WHERE parent_role_id = $1", s.roleID["trust_lead"])
	if err != nil {
		t.Fatal(err)
	}
	if allowed("s_trust_lead", authz.ActionPlatformUserRead) {
		t.Error("user.read still held after TRUST_LEAD's inclusions were removed")
	}
}

func constraintOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// The role graph's invariants hold whoever writes (ADR-0026): no cycle,
// separation of duties for roles and for staff (OWNER exempt), and
// disabling an account moves its token version.
func TestRoleGraphInvariants(t *testing.T) {
	t.Parallel()
	s := newStaffDB(t)
	ctx := t.Context()
	include := func(parent, child string) error {
		_, err := s.owner.Pool().Exec(ctx,
			"INSERT INTO staff_role_includes (parent_role_id, child_role_id, created_at) VALUES ($1, $2, now())",
			s.roleID[parent], s.roleID[child])
		return err
	}
	assign := func(staff, role string) error {
		_, err := s.maint.Pool().Exec(ctx,
			"INSERT INTO staff_role_assignments (staff_id, role_id, created_at) VALUES ($1, $2, now())",
			fixtureID(staff), s.roleID[role])
		return err
	}
	tests := []struct {
		name, constraint string
		write            func() error
	}{
		{"a cycle through the DAG", "staff_role_includes_acyclic", func() error { return include("support_agent", "trust_lead") }},
		{"a role reaching both branches", "staff_separation_of_duties", func() error { return include("finance_analyst", "moderator") }},
		{"a member holding both branches", "staff_separation_of_duties", func() error { return assign("s_moderator", "finance_analyst") }},
		{"a self edge", "staff_role_includes_no_self_edge", func() error { return include("auditor", "auditor") }},
	}
	for _, tt := range tests {
		if got := constraintOf(tt.write()); got != tt.constraint {
			t.Errorf("%s: constraint %q, want %q", tt.name, got, tt.constraint)
		}
	}
	// OWNER is exempt: its holder may also hold a role of either branch.
	if err := assign("s_owner", "finance_analyst"); err != nil {
		t.Errorf("OWNER with FINANCE_ANALYST: %v", err)
	}
	if err := assign("s_owner", "moderator"); err != nil {
		t.Errorf("OWNER with MODERATOR: %v", err)
	}

	var before, after int64
	q := s.maint.Pool()
	if err := q.QueryRow(ctx, "SELECT token_version FROM staff_members WHERE id = $1", fixtureID("s_auditor")).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(ctx, "UPDATE staff_members SET status = 'DISABLED' WHERE id = $1", fixtureID("s_auditor")); err != nil {
		t.Fatal(err)
	}
	if err := q.QueryRow(ctx, "SELECT token_version FROM staff_members WHERE id = $1", fixtureID("s_auditor")).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Errorf("token version %d → %d after disabling, want it moved (ADR-0025 §6)", before, after)
	}
}

// TestPermissionCatalogMatchesDatabase: the CHECK of
// staff_role_permissions lists exactly the code's permission catalog.
func TestPermissionCatalogMatchesDatabase(t *testing.T) {
	t.Parallel()
	owner := dbtest.New(t).Open(t, db.RoleOwner)
	var def string
	err := owner.Pool().QueryRow(context.Background(), `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = 'staff_role_permissions_permission_check'`).Scan(&def)
	if err != nil {
		t.Fatal(err)
	}
	var inDB []string
	for _, m := range regexp.MustCompile(`'([a-z_.]+)'`).FindAllStringSubmatch(def, -1) {
		inDB = append(inDB, m[1])
	}
	var inCode []string
	for _, p := range authz.Permissions() {
		inCode = append(inCode, string(p.Code))
	}
	slices.Sort(inDB)
	if !slices.Equal(inDB, inCode) {
		t.Errorf("CHECK permissions = %v, catalog = %v", inDB, inCode)
	}
}
