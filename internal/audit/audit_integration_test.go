//go:build integration

package audit_test

import (
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
	rec := audit.NewRecorder(clock.NewFake(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)))

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
	rec := audit.NewRecorder(clock.Real{})
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

// TestPurge: `koala maint audit-purge` deletes, as koala_maint, the rows
// whose retention has passed and keeps the rest; a FINANCIAL entry
// outlives a normal one.
func TestPurge(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	app, maint := d.Open(t, db.RoleApp), d.Open(t, db.RoleMaint)
	ctx := t.Context()
	// The database's clock decides the retention: the old rows are three
	// years older than it.
	old := audit.NewRecorder(clock.NewFake(clock.Real{}.Now().AddDate(-3, 0, 0)))
	fresh := audit.NewRecorder(clock.Real{})

	normal, financial := entry(authz.SensitivityNormal), entry(authz.SensitivityFinancial)
	oldPending, err := old.Pending(ctx, app.Pool(), normal)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Result(ctx, app.Pool(), oldPending, normal, true); err != nil {
		t.Fatal(err)
	}
	if err := old.Detail(ctx, app.Pool(), oldPending, map[string]any{"note": "x"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Pending(ctx, app.Pool(), financial); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Pending(ctx, app.Pool(), normal); err != nil {
		t.Fatal(err)
	}

	p, err := audit.Purge(ctx, maint, clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Details != 1 || p.Entries != 2 {
		t.Errorf("purged %+v, want the old detail and the old pending and result rows", p)
	}
	var left int
	if err := maint.Pool().QueryRow(ctx, "SELECT count(*) FROM admin_audit_entries").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 2 {
		t.Errorf("%d entries left, want the old FINANCIAL one and the fresh one", left)
	}
	if _, err := audit.Purge(ctx, app, clock.Real{}); err == nil {
		t.Error("the application role purged the audit trail")
	}
}
