// Package softdeletetest is the generated read test of the soft-delete
// platform (ADR-0013, ADR-0020; plan §6.1 Faz 1, "üretilmiş okuma testi").
//
// Every GET operation of the contract names the table it reads with
// `x-resource: <table>`. ReadTest walks those operations and, for every
// soft-deleted table among them, creates an object with the table's
// fixture, checks that each operation shows it, soft-deletes it with
// softdelete.Delete and checks again:
//
//   - a detail operation (one path parameter) answers 404 with exactly the
//     body it gives for an id that never existed (a deleted object and a
//     missing one are indistinguishable, ADR-0014);
//   - a list operation (no path parameter) no longer contains the id.
//
// An operation marked `x-resource-self: true` reads the caller itself
// (/v1/me): before the deletion it answers 200 with the id, after it the
// caller no longer authenticates (401) and the id does not appear.
// `x-resource: none` marks a GET that reads no table (ADR-0026).
//
// A new operation or table joins the test by itself; a soft-deleted table
// without a fixture fails it.
package softdeletetest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// Extension is the operation extension that names the table an operation
// reads.
const Extension = "x-resource"

// ExtensionSelf marks an operation that reads the caller itself.
const ExtensionSelf = "x-resource-self"

// None is the Extension value of a GET that reads no table.
const None = "none"

// requestID is sent with every request, so two problem bodies compare
// byte for byte.
const requestID = "softdeletetest"

// Fixture creates one active row of its table through q (koala_app, the
// active views) and returns its id.
type Fixture func(ctx context.Context, q db.Querier) (uuid.UUID, error)

// ReadTest is the generated read test over one contract and handler.
type ReadTest struct {
	// Spec is the contract; its GET operations carry x-resource.
	Spec *openapi3.T
	// Handler serves Spec.
	Handler http.Handler
	// App is a koala_app pool on the database Handler reads.
	App *db.DB
	// Catalog classifies the tables.
	Catalog *softdelete.Catalog
	// Fixtures maps every soft-deleted table that an operation reads to
	// its fixture.
	Fixtures map[string]Fixture
	// Authorize, when set, prepares each request about the object id of
	// table: the credentials of the caller that may read it (for a self
	// operation, the object itself) and the client headers. It is called
	// before and after the deletion with the same id.
	Authorize func(ctx context.Context, table string, id uuid.UUID, r *http.Request) error
}

// operation is one GET operation of the contract.
type operation struct {
	path   string
	params int
	table  string
	self   bool
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// Run runs the test, one subtest per soft-deleted table.
func (rt ReadTest) Run(t *testing.T) {
	t.Helper()
	byTable, problems := rt.plan()
	for _, p := range problems {
		t.Error(p)
	}
	if len(problems) > 0 {
		t.FailNow()
	}
	for _, table := range slices.Sorted(maps.Keys(byTable)) {
		ops := byTable[table]
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			rt.runTable(t, table, ops)
		})
	}
}

// plan checks the contract against the catalog and the fixtures, without a
// database, and returns the operations of every soft-deleted table and the
// problems found: an x-resource that is not a table name or not in the
// catalog, a soft-deleted table without a fixture, an operation with more
// than one path parameter, or no operation with x-resource at all. Exempt
// tables have nothing to hide and are left out.
func (rt ReadTest) plan() (map[string][]operation, []string) {
	out := map[string][]operation{}
	var problems []string
	seen := false
	for _, path := range slices.Sorted(maps.Keys(rt.Spec.Paths.Map())) {
		op := rt.Spec.Paths.Value(path).Get
		if op == nil {
			continue
		}
		raw, ok := op.Extensions[Extension]
		if !ok {
			continue
		}
		seen = true
		table, ok := extensionString(raw)
		if !ok || table == "" {
			problems = append(problems, fmt.Sprintf("GET %s: %s must be a table name, got %v", path, Extension, raw))
			continue
		}
		if table == None {
			continue
		}
		tb, ok := rt.Catalog.Table(table)
		if !ok {
			problems = append(problems, fmt.Sprintf("GET %s: %s %s is not in the soft-delete catalog", path, Extension, table))
			continue
		}
		if !tb.SoftDelete() {
			continue
		}
		params := len(pathParam.FindAllString(path, -1))
		if params > 1 {
			problems = append(problems, fmt.Sprintf("GET %s: operations with %d path parameters are not supported yet; extend softdeletetest", path, params))
			continue
		}
		self := extensionTrue(op.Extensions[ExtensionSelf])
		if self && params > 0 {
			problems = append(problems, fmt.Sprintf("GET %s: a %s operation has no path parameter", path, ExtensionSelf))
			continue
		}
		out[table] = append(out[table], operation{path: path, params: params, table: table, self: self})
	}
	if !seen {
		problems = append(problems, fmt.Sprintf("no GET operation carries %s", Extension))
	}
	for _, table := range slices.Sorted(maps.Keys(out)) {
		if _, ok := rt.Fixtures[table]; !ok {
			problems = append(problems, fmt.Sprintf("no fixture for the soft-deleted table %s", table))
		}
	}
	return out, problems
}

// extensionString reads a string extension value; kin-openapi keeps
// extension values either decoded or as raw JSON.
func extensionString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.RawMessage:
		var s string
		if err := json.Unmarshal(x, &s); err != nil {
			return "", false
		}
		return s, true
	default:
		return "", false
	}
}

// extensionTrue reads a boolean extension value, decoded or raw JSON.
func extensionTrue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case json.RawMessage:
		var b bool
		return json.Unmarshal(x, &b) == nil && b
	default:
		return false
	}
}

func (rt ReadTest) runTable(t *testing.T, table string, ops []operation) {
	t.Helper()
	ctx := t.Context()
	var id uuid.UUID
	err := rt.App.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		var err error
		if id, err = rt.Fixtures[table](ctx, q); err != nil {
			return fmt.Errorf("fixture: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fixture %s: %v", table, err)
	}
	// Before the deletion every operation shows the object, so the checks
	// after it cannot pass by accident.
	for _, op := range ops {
		if err := rt.visibility(ctx, op, id, true); err != nil {
			t.Error(err)
		}
	}

	err = rt.App.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		_, err := softdelete.New(rt.Catalog).Delete(ctx, q, softdelete.DeleteRequest{
			Table: table, ID: id,
			Actor:  softdelete.Actor{Kind: softdelete.ActorSystem},
			Reason: softdelete.ReasonRetention, At: time.Now(),
		})
		if err != nil {
			return fmt.Errorf("soft-delete: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("delete %s %s: %v", table, id, err)
	}
	for _, op := range ops {
		if err := rt.visibility(ctx, op, id, false); err != nil {
			t.Error(err)
		}
	}
}

// visibility checks that op shows (visible) or hides the object id and
// describes the failure.
func (rt ReadTest) visibility(ctx context.Context, op operation, id uuid.UUID, visible bool) error {
	if op.self {
		status, body := rt.get(ctx, op, op.path, id)
		switch {
		case visible && (status != http.StatusOK || !strings.Contains(body, id.String())):
			return fmt.Errorf("GET %s as the object itself = %d %s, want 200 with its id", op.path, status, body)
		case !visible && (status != http.StatusUnauthorized || strings.Contains(body, id.String())):
			return fmt.Errorf("GET %s as the deleted object = %d %s, want 401 without its id", op.path, status, body)
		}
		return nil
	}
	if op.params == 0 {
		status, body := rt.get(ctx, op, op.path, id)
		if status != http.StatusOK {
			return fmt.Errorf("GET %s = %d, want 200: %s", op.path, status, body)
		}
		if got := strings.Contains(body, id.String()); got != visible {
			return fmt.Errorf("GET %s lists %s: %v, want %v", op.path, id, got, visible)
		}
		return nil
	}

	status, body := rt.get(ctx, op, pathParam.ReplaceAllString(op.path, id.String()), id)
	if visible {
		if status != http.StatusOK {
			return fmt.Errorf("GET %s for an active object = %d, want 200: %s", op.path, status, body)
		}
		return nil
	}
	missingStatus, missingBody := rt.get(ctx, op, pathParam.ReplaceAllString(op.path, uuid.NewString()), id)
	if status != http.StatusNotFound || status != missingStatus || body != missingBody {
		return fmt.Errorf("GET %s for a deleted object = %d %s; for a missing one = %d %s; want the same 404",
			op.path, status, body, missingStatus, missingBody)
	}
	return nil
}

// get requests path as the caller Authorize sets up for the object id.
func (rt ReadTest) get(ctx context.Context, op operation, path string, id uuid.UUID) (int, string) {
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
	req.Header.Set(httpx.HeaderRequestID, requestID)
	if rt.Authorize != nil {
		if err := rt.Authorize(ctx, op.table, id, req); err != nil {
			return 0, "authorize: " + err.Error()
		}
	}
	rec := httptest.NewRecorder()
	rt.Handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}
