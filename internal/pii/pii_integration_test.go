//go:build integration

package pii_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/authzsql"
	"github.com/is-koala/koala-api/internal/pii"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
)

type piiEnv struct {
	app, maint *db.DB
	gate       *pii.Gate
	lead, none uuid.UUID
}

func newPIIEnv(t *testing.T) *piiEnv {
	t.Helper()
	d := dbtest.New(t)
	e := &piiEnv{app: d.Open(t, db.RoleApp), maint: d.Open(t, db.RoleMaint), lead: uuid.New(), none: uuid.New()}
	model, err := authz.DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	e.gate = pii.NewGate(authzsql.NewStaffChecker(model, e.app.Pool()), clock.Real{})
	for id, email := range map[uuid.UUID]string{e.lead: "lead@iskoala.com", e.none: "none@iskoala.com"} {
		_, err := e.maint.Pool().Exec(t.Context(), `
			INSERT INTO staff_members (id, email, password_hash, status, must_change_password, created_at)
			VALUES ($1, $2, '$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaA', 'ACTIVE', false, $3)`,
			id, email, time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
	}
	// TRUST_LEAD holds user.read_pii.
	_, err = e.maint.Pool().Exec(t.Context(), `
		INSERT INTO staff_role_assignments (staff_id, role_id, created_at)
		SELECT $1, id, now() FROM staff_roles WHERE code = 'TRUST_LEAD'`, e.lead)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *piiEnv) logRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.maint.Pool().QueryRow(t.Context(), "SELECT count(*) FROM pii_access_log").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestGateGrantsWithItsLogRow: a grant exists only with its
// pii_access_log row, written in the reading transaction, with the basis
// of the user.read_pii decision; it opens the granted fields of the data
// subject and nothing else (plan §5.5, ADR-0026).
func TestGateGrantsWithItsLogRow(t *testing.T) {
	t.Parallel()
	e := newPIIEnv(t)
	subject := uuid.New()
	phone := pii.NewValue(pii.FieldPhone, subject, "+905551112233")
	tckn := pii.NewValue(pii.FieldTCKN, subject, "10000000146")
	otherPhone := pii.NewValue(pii.FieldPhone, uuid.New(), "+905559998877")
	err := e.app.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		g, err := e.gate.Open(ctx, q, authz.Staff(e.lead), subject, "identity_review", []pii.Field{pii.FieldPhone})
		if err != nil || g == nil {
			t.Fatalf("Open = %v, %v; want a grant", g, err)
		}
		if v, ok := pii.Reveal(g, phone); !ok || v != "+905551112233" {
			t.Errorf("granted field: %q, %v", v, ok)
		}
		if _, ok := pii.Reveal(g, tckn); ok {
			t.Error("the grant opened a field it was not given")
		}
		if _, ok := pii.Reveal(g, otherPhone); ok {
			t.Error("the grant opened another data subject's field")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var basis string
	if err := e.maint.Pool().QueryRow(t.Context(), "SELECT basis FROM pii_access_log WHERE staff_id = $1", e.lead).Scan(&basis); err != nil {
		t.Fatal(err)
	}
	if basis == "" {
		t.Error("the log row carries no basis")
	}

	err = e.app.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		g, err := e.gate.Open(ctx, q, authz.Staff(e.none), subject, "identity_review", []pii.Field{pii.FieldPhone})
		if err != nil || g != nil {
			t.Fatalf("Open without user.read_pii = %v, %v; want no grant and no error (masked answer)", g, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.logRows(t); n != 1 {
		t.Errorf("%d log rows, want the grant's only", n)
	}
}

// TestNoLogRowNoGrant: when the access log row cannot be written there is
// no grant, and the reading transaction survives to answer masked. The
// koala_maint role may not insert into the log, which makes the write
// fail here.
func TestNoLogRowNoGrant(t *testing.T) {
	t.Parallel()
	e := newPIIEnv(t)
	err := e.maint.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		g, err := e.gate.Open(ctx, q, authz.Staff(e.lead), uuid.New(), "identity_review", []pii.Field{pii.FieldPhone})
		if err == nil || g != nil {
			t.Fatalf("Open = %v, %v; want no grant and the write's error", g, err)
		}
		var one int
		if err := q.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Fatalf("the transaction did not survive the failed log write: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.logRows(t); n != 0 {
		t.Errorf("%d log rows after a failed write", n)
	}
}
