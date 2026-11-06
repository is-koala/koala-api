package staff_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/audit"
	"github.com/is-koala/koala-api/internal/audit/auditdb"
	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/staff"
)

// The gate is never consulted in these tests: nothing reaches a decision.
type noChecker struct{}

func (noChecker) Check(context.Context, authz.Subject, authz.Action, authz.Object, ...authz.CheckOption) (authz.Decision, error) {
	return authz.Decision{Allowed: true}, nil
}

type noRecorder struct{}

func (noRecorder) Pending(context.Context, auditdb.DBTX, audit.Entry) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (noRecorder) Result(context.Context, auditdb.DBTX, uuid.UUID, audit.Entry, bool) error {
	return nil
}

func (noRecorder) Record(context.Context, auditdb.DBTX, audit.Entry, audit.Outcome) (uuid.UUID, error) {
	return uuid.New(), nil
}

func loadSpec(t *testing.T, ops string) *openapi3.T {
	t.Helper()
	doc := `openapi: 3.0.3
info: {title: t, version: 1.0.0}
components:
  securitySchemes:
    adminBearer: {type: http, scheme: bearer}
    marketplaceBearer: {type: http, scheme: bearer}
security: [{marketplaceBearer: []}]
paths:
` + ops
	spec, err := openapi3.NewLoader().LoadFromData([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func newGate(spec *openapi3.T) (*staff.Gate, error) {
	return staff.NewGate(spec, noChecker{}, noRecorder{}, nil, slog.New(slog.DiscardHandler))
}

// The gate is fail-closed at startup (plan §5.5, ADR-0014 Karar 5,
// ADR-0026): a staff operation without a known permission or a documented
// exemption, or a permission outside the staff surface, stops the server.
func TestGateRefusesAnUnmappedStaffOperation(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ op, want string }{
		"no permission, no exemption": {`  /v1/admin/payouts:
    get:
      operationId: listPayouts
      security: [{adminBearer: []}]
      responses: {"200": {description: ok}}
`, "exactly one of"},
		"both": {`  /v1/admin/payouts:
    get:
      operationId: listPayouts
      security: [{adminBearer: []}]
      x-authz-permission: payout.reconcile
      x-authz-exempt: Personelin kendi hesabı; izin gerektiren nesne yok.
      responses: {"200": {description: ok}}
`, "exactly one of"},
		"unknown permission": {`  /v1/admin/payouts:
    get:
      operationId: listPayouts
      security: [{adminBearer: []}]
      x-authz-permission: payout.read_all
      responses: {"200": {description: ok}}
`, "is not a staff permission"},
		"exemption outside the self-service list": {`  /v1/admin/payouts:
    get:
      operationId: listPayouts
      security: [{adminBearer: []}]
      x-authz-exempt: Personelin kendi hesabı; izin gerektiren nesne yok.
      responses: {"200": {description: ok}}
`, "outside the self-service list"},
		"permission outside the staff surface": {`  /v1/jobs:
    get:
      operationId: listJobs
      x-authz-permission: job.read
      responses: {"200": {description: ok}}
`, "without adminBearer"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := newGate(loadSpec(t, tt.op))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewGate error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// Every staff operation of the contract maps (the server starts), and the
// permission catalog operation needs role.manage.
func TestGateMapsTheContract(t *testing.T) {
	t.Parallel()
	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g, err := newGate(spec)
	if err != nil {
		t.Fatalf("the contract has an unmapped staff operation: %v", err)
	}
	if p, ok := g.Permission("GET /v1/admin/permissions"); !ok || p != "role.manage" {
		t.Errorf("GET /v1/admin/permissions needs %q, %v; want role.manage", p, ok)
	}
	for _, self := range []string{"GET /v1/admin/me", "GET /v1/admin/me/capabilities", "POST /v1/admin/auth/login"} {
		if p, ok := g.Permission(self); ok {
			t.Errorf("%s needs %s; it is self-service or public", self, p)
		}
	}
}

// At request time the gate fails closed too: a guarded operation reached
// without a staff principal is a 500, never a pass, and a route the gate
// does not know is a 500.
func TestGateFailsClosedAtRequestTime(t *testing.T) {
	t.Parallel()
	g, err := newGate(loadSpec(t, `  /v1/admin/payouts:
    get:
      operationId: listPayouts
      security: [{adminBearer: []}]
      x-authz-permission: payout.reconcile
      responses: {"200": {description: ok}}
`))
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	r := chi.NewRouter()
	r.Group(func(gr chi.Router) {
		gr.Use(g.Handler)
		gr.Get("/v1/admin/payouts", ok)
		gr.Get("/v1/admin/unmapped", ok)
	})
	for _, path := range []string{"/v1/admin/payouts", "/v1/admin/unmapped"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody))
		if rec.Code != http.StatusInternalServerError || reached {
			t.Errorf("%s: status %d, handler reached %v; want 500 and no pass", path, rec.Code, reached)
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte(`"INTERNAL"`)) {
			t.Errorf("%s: body %s, want the INTERNAL problem", path, rec.Body)
		}
	}
}
