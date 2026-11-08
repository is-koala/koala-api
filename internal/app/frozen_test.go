package app

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/pending"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// extAuthz is the operation extension that names the authorization
// decision of a marketplace operation: {action: can_*, object: <type>}
// (stil rehberi §11).
const extAuthz = "x-authz"

// methodNames returns the exported method names of t.
func methodNames(t reflect.Type) []string {
	out := make([]string, 0, t.NumMethod())
	for i := range t.NumMethod() {
		out = append(out, t.Method(i).Name)
	}
	slices.Sort(out)
	return out
}

// TestPendingServerServesExactlyItsOperations binds pending.Operations(),
// the list the contract is held against, to the methods pending.Server
// actually has. Without it the list is 64 strings nothing checks: a stub
// deleted while its line stayed, or a method added without a line, would
// both pass (ADR-0029 §1).
func TestPendingServerServesExactlyItsOperations(t *testing.T) {
	t.Parallel()

	served := methodNames(reflect.TypeOf(pending.Server{}))
	contract := methodNames(reflect.TypeOf((*apiv1.StrictServerInterface)(nil)).Elem())
	for _, name := range served {
		if !slices.Contains(contract, name) {
			t.Errorf("pending.Server has %s, which is not an operation of the generated strict server interface", name)
		}
	}
	if len(served) != len(pending.Operations()) {
		t.Errorf("pending.Server has %d methods but pending.Operations() lists %d routes; they must describe the same operations",
			len(served), len(pending.Operations()))
	}
}

// TestFrozenOperationsAreNotShadowed closes the way around the freeze:
// strictServer embeds pending.Server, but a method declared directly on
// strictServer shadows the embedded one at depth 0. Writing such a
// forwarder for a frozen operation compiles, serves real data, and leaves
// every other gate green — the contract still says x-not-implemented, so
// ReadTest and JoinTest keep skipping the operation, and the two-way test
// still matches. So: no method declared on strictServer may be one
// pending.Server also has.
//
// The walk covers every file of this package, not only the one that
// declares strictServer today: a forwarder put in staff.go or in a new
// file would be exactly as invisible, and the file the type lives in is
// not where the rule applies.
func TestFrozenOperationsAreNotShadowed(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) == 0 {
		t.Fatal("no Go package parsed in this directory; the walk is looking in the wrong place")
	}
	frozen := methodNames(reflect.TypeOf(pending.Server{}))
	own, files := 0, 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			files++
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
					continue
				}
				// A value receiver is the only form that compiles here:
				// strictServer is passed by value to the generated
				// handler. A pointer receiver would not satisfy the
				// interface, so it cannot hide a forwarder.
				ident, ok := fd.Recv.List[0].Type.(*ast.Ident)
				if !ok || ident.Name != "strictServer" {
					continue
				}
				own++
				if slices.Contains(frozen, fd.Name.Name) {
					t.Errorf("%s: strictServer declares %s, which pending.Server also serves: the forwarder shadows the 501 stub while the contract still marks the operation frozen",
						fset.Position(fd.Pos()), fd.Name.Name)
				}
			}
		}
	}
	if files < 2 || own == 0 {
		t.Fatalf("walked %d file(s) and found %d strictServer method(s); the walk is looking in the wrong place", files, own)
	}
}

// TestFrozenOperationAnswers501 proves the claim the contract makes, over
// HTTP, on the real router: a frozen operation answers 501 with a problem
// document whose code is NOT_IMPLEMENTED. GET /v1/reference/countries is
// the witness because it is public and takes neither a body nor a
// parameter, so nothing before the handler can answer first.
func TestFrozenOperationAnswers501(t *testing.T) {
	t.Parallel()

	const route = "GET /v1/reference/countries"
	if !slices.Contains(pending.Operations(), route) {
		t.Fatalf("%s is no longer frozen; pick another public frozen operation as the witness", route)
	}
	h := newAPIRouter(t, config.EnvTest)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/reference/countries", http.NoBody)
	req.Header.Set(httpx.HeaderRequestID, "req-1")
	setClient(req.Header)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want a problem document", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"code":"NOT_IMPLEMENTED"`) {
		t.Errorf("body = %s, want the NOT_IMPLEMENTED code", body)
	}
}

// TestOperationAuthzNamesAModelRelation binds x-authz to the model. The
// lint only checks its shape (can_*), so an action no type defines — or
// one on the wrong type — passes every gate today, and the style guide's
// rule that `action` is a relation of `object` in model.fga is enforced
// nowhere. A marketplace decision that names nothing is a decision no
// evaluator can make.
func TestOperationAuthzNamesAModelRelation(t *testing.T) {
	t.Parallel()

	model, err := authz.DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, path := range slices.Sorted(maps.Keys(spec.Paths.Map())) {
		for method, op := range spec.Paths.Value(path).Operations() {
			raw, ok := op.Extensions[extAuthz]
			if !ok {
				continue
			}
			where := method + " " + path
			decision, ok := raw.(map[string]any)
			if !ok {
				t.Errorf("%s: %s is not an object, got %T", where, extAuthz, raw)
				continue
			}
			action, _ := decision["action"].(string)
			object, _ := decision["object"].(string)
			if action == "" || object == "" {
				t.Errorf("%s: %s needs an action and an object, got %v", where, extAuthz, decision)
				continue
			}
			seen++
			relations := model.Relations(object)
			if relations == nil {
				t.Errorf("%s: %s names object type %q, which model.fga does not define", where, extAuthz, object)
				continue
			}
			if !slices.Contains(relations, action) {
				t.Errorf("%s: %s names %s#%s, which model.fga does not define (relations of %s: %v)",
					where, extAuthz, object, action, object, relations)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no operation carried x-authz; the walker misses them")
	}
}
