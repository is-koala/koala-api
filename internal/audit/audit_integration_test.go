//go:build integration

package audit_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is-koala/koala-api/internal/audit"
	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
)

func entry(s authz.Sensitivity) audit.Entry {
	return audit.Entry{
		Actor: authz.Staff(uuid.New()), Action: "payout.release", Permission: "payout.release",
		Basis:     authz.Basis{{Steps: []authz.Step{{Object: authz.Platform(), Relation: "payout_release"}}, Grant: authz.TupleUser{Type: "staff", ID: "x"}}},
		Target:    authz.Object{Type: "job_assignment", ID: uuid.NewString()},
		RequestID: "req-audit", Mutating: true, Sensitivity: s,
	}
}

// interval turns a catalog period ("P24M", "P10Y") into a Postgres
// interval.
func interval(t *testing.T, period string) string {
	t.Helper()
	n, err := strconv.Atoi(period[1 : len(period)-1])
	if err != nil {
		t.Fatalf("period %q: %v", period, err)
	}
	if strings.HasSuffix(period, "Y") {
		return strconv.Itoa(n) + " years"
	}
	return strconv.Itoa(n) + " months"
}

// TestRetentionMatchesCatalog: the database computes each row's
// retain_until from the periods db/softdelete.yaml records for the M1
// tables (ADR-0026): an entry 24 months, a FINANCIAL entry 10 years, the
// result row its pending row's, a detail 6 months.
func TestRetentionMatchesCatalog(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	app, owner := d.Open(t, db.RoleApp), d.Open(t, db.RoleOwner)
	ctx := t.Context()
	cat := dbtest.SoftDeleteCatalog(t)
	entries, _ := cat.Table("admin_audit_entries")
	details, _ := cat.Table("admin_audit_entry_details")
	rec := audit.NewRecorder()

	normal := entry(authz.SensitivityPersonalData)
	pending, err := rec.Pending(ctx, app.Pool(), normal)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Result(ctx, app.Pool(), pending, normal, true); err != nil {
		t.Fatal(err)
	}
	if err := rec.Detail(ctx, app.Pool(), pending, map[string]any{"iban": "TR330006100519786457841326"}, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	financial := entry(authz.SensitivityFinancial)
	if _, err := rec.Pending(ctx, app.Pool(), financial); err != nil {
		t.Fatal(err)
	}

	check := func(what, sql, period string, args ...any) {
		t.Helper()
		var ok bool
		if err := owner.Pool().QueryRow(ctx, sql, append([]any{interval(t, period)}, args...)...).Scan(&ok); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if !ok {
			t.Errorf("%s: retain_until is not created_at + %s", what, period)
		}
	}
	check("entry", `SELECT bool_and(retain_until = created_at + $1::interval) FROM admin_audit_entries
		WHERE sensitivity <> 'FINANCIAL' AND result_of IS NULL`, entries.Retention["entry"])
	check("financial entry", `SELECT bool_and(retain_until = created_at + $1::interval) FROM admin_audit_entries
		WHERE sensitivity = 'FINANCIAL'`, entries.Retention["financial_entry"])
	check("result row", `SELECT r.retain_until = p.created_at + $1::interval FROM admin_audit_entries r
		JOIN admin_audit_entries p ON p.id = r.result_of WHERE r.result_of = $2`, entries.Retention["entry"], pending)
	check("detail", `SELECT bool_and(retain_until = created_at + $1::interval) FROM admin_audit_entry_details`,
		details.Retention["detail"])

	var body string
	if err := owner.Pool().QueryRow(ctx, "SELECT body::text FROM admin_audit_entry_details").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "TR33") {
		t.Errorf("detail body %s carries the IBAN", body)
	}
}

func constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// The audit trail is immutable (plan §5.5, ADR-0026): no role updates a
// row, not even the owner; only koala_maint deletes, and only rows past
// their retention; nobody truncates. A result must close a pending row of
// the same actor and action.
func TestAuditRowsAreImmutable(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	app, owner, maint := d.Open(t, db.RoleApp), d.Open(t, db.RoleOwner), d.Open(t, db.RoleMaint)
	ctx := t.Context()
	rec := audit.NewRecorder()
	e := entry(authz.SensitivityNormal)
	pending, err := rec.Pending(ctx, app.Pool(), e)
	if err != nil {
		t.Fatal(err)
	}

	for name, sql := range map[string]string{
		"owner update":        "UPDATE admin_audit_entries SET outcome = 'SUCCEEDED'",
		"owner delete":        "DELETE FROM admin_audit_entries",
		"owner truncate":      "TRUNCATE admin_audit_entries CASCADE",
		"pii log truncate":    "TRUNCATE pii_access_log",
		"change log truncate": "TRUNCATE authz_change_log",
	} {
		_, err := owner.Pool().Exec(ctx, sql)
		if c := constraint(err); !strings.HasSuffix(c, "_immutable") {
			t.Errorf("%s: err %v (constraint %q), want the evidence trigger's refusal", name, err, c)
		}
	}
	if _, err := maint.Pool().Exec(ctx, "DELETE FROM admin_audit_entries WHERE id = $1", pending); constraint(err) != "admin_audit_entries_immutable" {
		t.Errorf("maint deleted a row within its retention: %v", err)
	}

	other := e
	other.Actor = authz.Staff(uuid.New())
	if err := rec.Result(ctx, app.Pool(), pending, other, true); constraint(err) != "admin_audit_entries_result_of_pending" {
		t.Errorf("a result of another actor closed the pending row: %v", err)
	}
	if err := rec.Result(ctx, app.Pool(), pending, e, true); err != nil {
		t.Fatal(err)
	}
	if err := rec.Result(ctx, app.Pool(), pending, e, false); constraint(err) != "admin_audit_entries_result_of_key" {
		t.Errorf("a pending row got a second result: %v", err)
	}
}

// backdate inserts an audit entry, its detail and a PII access row
// created years ago. The stamp triggers set created_at to the statement's
// time whatever the writer sends, so only the table owner, by disabling
// them for one transaction of the test's own database, can write the
// past (ADR-0026: the AU-9 boundary is the owner).
func backdate(t *testing.T, owner *db.DB, sensitivity string, created time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := owner.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		exec := func(sql string, args ...any) {
			t.Helper()
			if _, err := q.Exec(ctx, sql, args...); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		exec("ALTER TABLE admin_audit_entries DISABLE TRIGGER admin_audit_entries_stamp")
		exec("ALTER TABLE admin_audit_entry_details DISABLE TRIGGER admin_audit_entry_details_stamp")
		exec("ALTER TABLE pii_access_log DISABLE TRIGGER pii_access_log_stamp")
		retain := map[string]string{"FINANCIAL": "10 years", "NORMAL": "24 months"}[sensitivity]
		exec(`INSERT INTO admin_audit_entries (id, created_at, outcome, actor_kind, action, mutating, sensitivity, retain_until)
			VALUES ($1, $2, 'DENIED', 'system', 'payout.release', true, $3, $2 + $4::interval)`, id, created, sensitivity, retain)
		exec(`INSERT INTO admin_audit_entry_details (entry_id, created_at, body, retain_until)
			VALUES ($1, $2, '{}', $2 + interval '6 months')`, id, created)
		exec(`INSERT INTO pii_access_log (id, created_at, staff_id, basis, data_subject_id, purpose, fields, retain_until)
			VALUES ($1, $2, $1, 'basis', $1, 'identity_review', '{phone}', $2 + interval '24 months')`, uuid.New(), created)
		exec("ALTER TABLE admin_audit_entries ENABLE TRIGGER admin_audit_entries_stamp")
		exec("ALTER TABLE admin_audit_entry_details ENABLE TRIGGER admin_audit_entry_details_stamp")
		exec("ALTER TABLE pii_access_log ENABLE TRIGGER pii_access_log_stamp")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestPurge: `koala maint audit-purge` deletes, as koala_maint, the rows
// whose retention has passed by the database's clock and keeps the rest; a
// FINANCIAL entry outlives a normal one. The application role cannot
// purge.
func TestPurge(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	app, owner, maint := d.Open(t, db.RoleApp), d.Open(t, db.RoleOwner), d.Open(t, db.RoleMaint)
	ctx := t.Context()
	threeYearsAgo := clock.Real{}.Now().AddDate(-3, 0, 0)
	oldNormal := backdate(t, owner, "NORMAL", threeYearsAgo)
	oldFinancial := backdate(t, owner, "FINANCIAL", threeYearsAgo)
	if _, err := audit.NewRecorder().Pending(ctx, app.Pool(), entry(authz.SensitivityNormal)); err != nil {
		t.Fatal(err)
	}

	p, err := audit.Purge(ctx, maint, clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	// Both details (6 months) and both PII rows (24 months) are past their
	// retention; of the entries only the normal one (24 months).
	if p.Details != 2 || p.Entries != 1 || p.PIIAccess != 2 {
		t.Errorf("purged %+v, want 2 details, 1 entry and 2 PII access rows", p)
	}
	var left []uuid.UUID
	rows, err := maint.Pool().Query(ctx, "SELECT id FROM admin_audit_entries WHERE id = ANY($1)", []uuid.UUID{oldNormal, oldFinancial})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != oldFinancial {
		t.Errorf("old entries left %v, want only the FINANCIAL one %s", left, oldFinancial)
	}
	if _, err := audit.Purge(ctx, app, clock.Real{}); err == nil {
		t.Error("the application role purged the audit trail")
	}
}

// The writer's clock does not move a retention: the database stamps
// created_at with the statement's time, whatever the insert sends.
func TestCreatedAtIsTheDatabases(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	app, owner := d.Open(t, db.RoleApp), d.Open(t, db.RoleOwner)
	ctx := t.Context()
	id := uuid.New()
	_, err := app.Pool().Exec(ctx, `
		INSERT INTO admin_audit_entries (id, created_at, outcome, actor_kind, action, mutating, sensitivity, retain_until)
		VALUES ($1, '2000-01-01T00:00:00Z', 'DENIED', 'system', 'payout.release', true, 'NORMAL', '2000-01-02T00:00:00Z')`, id)
	if err != nil {
		t.Fatal(err)
	}
	var recent bool
	err = owner.Pool().QueryRow(ctx, `
		SELECT created_at > now() - interval '1 hour' AND retain_until = created_at + interval '24 months'
		FROM admin_audit_entries WHERE id = $1`, id).Scan(&recent)
	if err != nil {
		t.Fatal(err)
	}
	if !recent {
		t.Error("the database kept the writer's created_at or retain_until")
	}
}

// TestEvidenceTriggersAreEnabled: the triggers that keep the evidence
// tables immutable and stamped, and the role graph's invariants, exist and
// are enabled after migration. The table owner can disable a trigger
// (ADR-0026, AU-9 boundary); a migration that does so fails here.
func TestEvidenceTriggersAreEnabled(t *testing.T) {
	t.Parallel()
	owner := dbtest.New(t).Open(t, db.RoleOwner)
	want := []string{
		"admin_audit_entries_immutable", "admin_audit_entries_no_truncate", "admin_audit_entries_stamp",
		"admin_audit_entry_details_immutable", "admin_audit_entry_details_no_truncate", "admin_audit_entry_details_stamp",
		"authz_change_log_immutable", "authz_change_log_no_truncate",
		"pii_access_log_immutable", "pii_access_log_no_truncate", "pii_access_log_stamp",
		"staff_members_disable_bumps_token_version",
		"staff_role_assignments_check", "staff_role_assignments_lock",
		"staff_role_includes_check", "staff_role_includes_lock",
		"staff_roles_check", "staff_roles_lock",
	}
	for _, name := range want {
		var enabled string
		err := owner.Pool().QueryRow(t.Context(),
			"SELECT tgenabled::text FROM pg_trigger WHERE tgname = $1 AND NOT tgisinternal", name).Scan(&enabled)
		if err != nil {
			t.Errorf("trigger %s: %v", name, err)
			continue
		}
		if enabled != "O" {
			t.Errorf("trigger %s: tgenabled %q, want O (enabled)", name, enabled)
		}
	}
}
