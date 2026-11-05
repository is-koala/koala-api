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
}

// operation is one GET operation of the contract.
type operation struct {
	path   string
	params int
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// Run runs the test, one subtest per table.
func (rt ReadTest) Run(t *testing.T) {
	t.Helper()
	byTable := rt.operations(t)
	if len(byTable) == 0 {
		t.Fatalf("no GET operation carries %s", Extension)
	}
	for _, table := range slices.Sorted(maps.Keys(byTable)) {
		ops := byTable[table]
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			rt.runTable(t, table, ops)
		})
	}
}

func (rt ReadTest) operations(t *testing.T) map[string][]operation {
	t.Helper()
	out := map[string][]operation{}
	for path, item := range rt.Spec.Paths.Map() {
		op := item.Get
		if op == nil {
			continue
		}
		raw, ok := op.Extensions[Extension]
		if !ok {
			continue
		}
		table, ok := extensionString(raw)
		if !ok || table == "" {
			t.Errorf("GET %s: %s must be a table name, got %v", path, Extension, raw)
			continue
		}
		out[table] = append(out[table], operation{path: path, params: len(pathParam.FindAllString(path, -1))})
	}
	return out
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

func (rt ReadTest) runTable(t *testing.T, table string, ops []operation) {
	t.Helper()
	tb, ok := rt.Catalog.Table(table)
	if !ok {
		t.Fatalf("%s %s is not in the soft-delete catalog", Extension, table)
	}
	if !tb.SoftDelete() {
		t.Logf("%s is exempt (%s); nothing to hide", table, tb.Class)
		return
	}
	fixture, ok := rt.Fixtures[table]
	if !ok {
		t.Fatalf("no fixture for the soft-deleted table %s", table)
	}
	for _, op := range ops {
		if op.params > 1 {
			t.Fatalf("GET %s: operations with %d path parameters are not supported yet; extend softdeletetest", op.path, op.params)
		}
	}

	ctx := t.Context()
	var id uuid.UUID
	err := rt.App.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		var err error
		if id, err = fixture(ctx, q); err != nil {
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
		rt.check(t, op, id, true)
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
		rt.check(t, op, id, false)
	}
}

// check asserts that op shows (visible) or hides the object id.
func (rt ReadTest) check(t *testing.T, op operation, id uuid.UUID, visible bool) {
	t.Helper()
	if op.params == 0 {
		status, body := rt.get(t, op.path)
		if status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: %s", op.path, status, body)
			return
		}
		if got := strings.Contains(body, id.String()); got != visible {
			t.Errorf("GET %s lists %s: %v, want %v", op.path, id, got, visible)
		}
		return
	}

	status, body := rt.get(t, pathParam.ReplaceAllString(op.path, id.String()))
	if visible {
		if status != http.StatusOK {
			t.Errorf("GET %s for an active object = %d, want 200: %s", op.path, status, body)
		}
		return
	}
	missingStatus, missingBody := rt.get(t, pathParam.ReplaceAllString(op.path, uuid.NewString()))
	if status != http.StatusNotFound || status != missingStatus || body != missingBody {
		t.Errorf("GET %s for a deleted object = %d %s; for a missing one = %d %s; want the same 404",
			op.path, status, body, missingStatus, missingBody)
	}
}

func (rt ReadTest) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody)
	req.Header.Set(httpx.HeaderRequestID, requestID)
	rec := httptest.NewRecorder()
	rt.Handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}
