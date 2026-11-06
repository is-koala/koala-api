//go:build integration

package staff_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is-koala/koala-api/internal/audit"
	"github.com/is-koala/koala-api/internal/audit/auditdb"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/authzsql"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/staff"
)

// gateSpec has one staff operation of each kind the gate audits: a
// mutation, a sensitive read and a plain read.
const gateSpec = `  /v1/admin/settings:
    post:
      operationId: updateSettings
      security: [{adminBearer: []}]
      x-authz-permission: settings.manage
      responses: {"200": {description: ok}}
  /v1/admin/payments:
    get:
      operationId: listPayments
      security: [{adminBearer: []}]
      x-authz-permission: payment.read
      responses: {"200": {description: ok}}
  /v1/admin/categories:
    get:
      operationId: listCategories
      security: [{adminBearer: []}]
      x-authz-permission: category.manage
      responses: {"200": {description: ok}}
`

// failingDB is a database that refuses every write: the pending row
// cannot be written.
type failingDB struct{}

var errDown = errors.New("database down")

func (failingDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errDown
}

func (failingDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errDown }

func (failingDB) QueryRow(context.Context, string, ...any) pgx.Row { return errRow{} }

type errRow struct{}

func (errRow) Scan(...any) error { return errDown }

type gateEnv struct {
	app, owner   *db.DB
	owner1, none uuid.UUID
	checker      *authzsql.StaffChecker
	spec         *openapi3.T
}

func newGateEnv(t *testing.T) *gateEnv {
	t.Helper()
	d := dbtest.New(t)
	e := &gateEnv{app: d.Open(t, db.RoleApp), owner: d.Open(t, db.RoleOwner), owner1: uuid.New(), none: uuid.New()}
	maint := d.Open(t, db.RoleMaint)
	for id, email := range map[uuid.UUID]string{e.owner1: "owner@iskoala.com", e.none: "none@iskoala.com"} {
		_, err := maint.Pool().Exec(t.Context(), `
			INSERT INTO staff_members (id, email, password_hash, status, must_change_password, created_at)
			VALUES ($1, $2, '$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaA', 'ACTIVE', false, $3)`,
			id, email, time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := maint.Pool().Exec(t.Context(), `
		INSERT INTO staff_role_assignments (staff_id, role_id, created_at)
		SELECT $1, id, now() FROM staff_roles WHERE code = 'OWNER'`, e.owner1)
	if err != nil {
		t.Fatal(err)
	}
	model, err := authz.DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	e.checker = authzsql.NewStaffChecker(model, e.app.Pool())
	e.spec = loadSpec(t, gateSpec)
	return e
}

// router serves the gate on writes; the handlers answer the status the
// request asks for (X-Test-Status) and count their runs.
func (e *gateEnv) router(t *testing.T, writes auditdb.DBTX, runs *int) http.Handler {
	t.Helper()
	g, err := staff.NewGate(e.spec, e.checker, audit.NewRecorder(), writes, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*runs++
		status, err := strconv.Atoi(r.Header.Get("X-Test-Status"))
		if err != nil {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	})
	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Group(func(gr chi.Router) {
		gr.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id := uuid.MustParse(r.Header.Get("X-Test-Staff"))
				next.ServeHTTP(w, r.WithContext(authn.ContextWithStaff(r.Context(), authn.StaffPrincipal{StaffID: id})))
			})
		}, g.Handler)
		gr.Post("/v1/admin/settings", handler)
		gr.Get("/v1/admin/payments", handler)
		gr.Get("/v1/admin/categories", handler)
	})
	return r
}

func call(h http.Handler, method, path string, staffID uuid.UUID, requestID string, status int) int {
	req := httptest.NewRequestWithContext(context.Background(), method, path, http.NoBody)
	req.Header.Set("X-Test-Staff", staffID.String())
	req.Header.Set(httpx.HeaderRequestID, requestID)
	req.Header.Set("X-Test-Status", strconv.Itoa(status))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

type row struct {
	ID       uuid.UUID
	Outcome  string
	ResultOf *uuid.UUID
	Mutating bool
	Basis    *string
}

// rows reads the audit rows of a request as the owner (the application
// cannot read the trail).
func (e *gateEnv) rows(t *testing.T, requestID string) []row {
	t.Helper()
	rs, err := e.owner.Pool().Query(t.Context(), `
		SELECT id, outcome, result_of, mutating, basis FROM admin_audit_entries
		WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rs, pgx.RowToStructByPos[row])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The gate's audit paths on the real checker and recorder (ADR-0026): a
// mutation gets a pending row before the handler and a result row naming
// it after; a sensitive read one row; a plain read none; a refusal one
// DENIED row and no handler.
func TestGateAuditsOnTheDatabase(t *testing.T) {
	t.Parallel()
	e := newGateEnv(t)
	runs := 0
	h := e.router(t, e.app.Pool(), &runs)

	tests := []struct {
		name, method, path, requestID string
		staff                         uuid.UUID
		status, wantStatus            int
		want                          []string // outcomes in order
		wantRuns                      int
	}{
		{"mutation succeeds", http.MethodPost, "/v1/admin/settings", "req-ok", e.owner1, 200, 200, []string{"PENDING", "SUCCEEDED"}, 1},
		{"mutation fails", http.MethodPost, "/v1/admin/settings", "req-conflict", e.owner1, 409, 409, []string{"PENDING", "FAILED"}, 1},
		{"sensitive read", http.MethodGet, "/v1/admin/payments", "req-read", e.owner1, 200, 200, []string{"SUCCEEDED"}, 1},
		{"plain read", http.MethodGet, "/v1/admin/categories", "req-plain", e.owner1, 200, 200, nil, 1},
		{"refusal", http.MethodPost, "/v1/admin/settings", "req-denied", e.none, 200, 403, []string{"DENIED"}, 0},
	}
	for i := range tests {
		tt := &tests[i]
		before := runs
		if got := call(h, tt.method, tt.path, tt.staff, tt.requestID, tt.status); got != tt.wantStatus {
			t.Errorf("%s: status %d, want %d", tt.name, got, tt.wantStatus)
		}
		if runs-before != tt.wantRuns {
			t.Errorf("%s: handler ran %d times, want %d", tt.name, runs-before, tt.wantRuns)
		}
		rows := e.rows(t, tt.requestID)
		var outcomes []string
		for _, r := range rows {
			outcomes = append(outcomes, r.Outcome)
		}
		if strings.Join(outcomes, ",") != strings.Join(tt.want, ",") {
			t.Errorf("%s: audit rows %v, want %v", tt.name, outcomes, tt.want)
			continue
		}
		switch {
		case len(rows) == 2 && (rows[1].ResultOf == nil || *rows[1].ResultOf != rows[0].ID):
			t.Errorf("%s: the result row does not name its pending row", tt.name)
		case len(rows) == 2 && (rows[0].Basis == nil || !strings.Contains(*rows[0].Basis, "staff_role:")):
			t.Errorf("%s: pending row basis %v, want the role path", tt.name, rows[0].Basis)
		case tt.name == "sensitive read" && (rows[0].Mutating || rows[0].ResultOf != nil):
			t.Errorf("%s: %+v, want one non-mutating row without result_of", tt.name, rows[0])
		case tt.name == "refusal" && rows[0].Basis != nil:
			t.Errorf("%s: a refusal carries a basis", tt.name)
		}
	}
}

// No pending row, no mutation (plan §5.5): when the pending row cannot be
// written the handler does not run and the answer is a 500.
func TestGateRunsNoMutationWithoutItsPendingRow(t *testing.T) {
	t.Parallel()
	e := newGateEnv(t)
	runs := 0
	h := e.router(t, failingDB{}, &runs)
	if got := call(h, http.MethodPost, "/v1/admin/settings", e.owner1, "req-down", 200); got != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", got)
	}
	if runs != 0 {
		t.Errorf("the handler ran %d times without a pending row", runs)
	}
}
