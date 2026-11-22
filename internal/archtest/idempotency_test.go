package archtest

import (
	"context"
	"go/ast"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/platform/idempotency"
)

// commitSite reports the fenced write of idempotency.Commit: the one
// KindConflict IDEMPOTENCY_KEY_IN_USE site of the idempotency package a
// handler can reach. The middleware answers the same code, but no handler
// calls the middleware, so only a call of Commit reaches this site.
func commitSite(s errorSite) bool {
	return s.kind == "KindConflict" && s.code == string(idempotency.CodeIdempotencyKeyInUse) &&
		strings.Contains(filepath.ToSlash(s.pos), "internal/platform/idempotency/")
}

// TestIdempotentHandlersCommitTheirKey is the static half of the binding
// of the Idempotency-Key store (ADR-0034 Karar 6). The operations come
// from the contract (idempotency.Operations): every one that takes the key
// and has a handler is bound, so a handler PR that lifts x-not-implemented
// joins this test by itself. For each, the handler's trace (the walk of
// TestHandlersReturnTheirDeclaredErrorCodes) must reach Commit's fenced
// write; a handler that answers a success without storing it in its
// transaction would repeat its side effect on a lost commit.
//
// The other side holds too: no handler of an operation without the key
// reaches Commit, so the site cannot be reached by a path every handler
// shares, which would keep this test green with any handler's Commit
// deleted.
//
// The walk cannot tell whether Commit runs inside the operation's
// transaction; the integration test TestIdempotentOperationsReplay in
// internal/app measures that on the database: the key's row and a
// business row the operation wrote carry the same xmin.
func TestIdempotentHandlersCommitTheirKey(t *testing.T) {
	t.Parallel()

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := idempotency.Operations(spec)
	if err != nil {
		t.Fatal(err)
	}
	keyed := map[string]bool{}
	for _, op := range ops {
		keyed[op.ID] = true
	}
	m := loadCodeModule(t)
	handlers := m.handlers()

	var bound, plain []string
	for _, path := range slices.Sorted(maps.Keys(spec.Paths.Map())) {
		for _, op := range spec.Paths.Value(path).Operations() {
			if _, frozen := op.Extensions["x-not-implemented"]; frozen {
				continue
			}
			name := strings.ToUpper(op.OperationID[:1]) + op.OperationID[1:]
			found := handlers[name]
			if len(found) != 1 {
				if keyed[op.OperationID] {
					// A keyed operation is never skipped: whatever serves it
					// must reach Commit, and only a strict handler can be traced.
					t.Errorf("%s takes an Idempotency-Key but has %d strict handlers, want 1", op.OperationID, len(found))
				}
				continue // a non-strict operation; the error binding test owns that case
			}
			sites, _, _ := m.trace(found[0])
			commits := slices.ContainsFunc(sites, commitSite)
			switch {
			case keyed[op.OperationID]:
				bound = append(bound, op.OperationID)
				if !commits {
					t.Errorf("%s takes an Idempotency-Key, but its handler never reaches idempotency.Commit: "+
						"store the success in the operation's transaction (ADR-0034 Karar 4)", op.OperationID)
				}
			case commits:
				t.Errorf("%s takes no Idempotency-Key, but its handler reaches idempotency.Commit", op.OperationID)
			default:
				plain = append(plain, op.OperationID)
			}
		}
	}
	// A check that saw nothing proves nothing.
	if !slices.Contains(bound, "completeRegistration") {
		t.Errorf("bound operations with a handler = %v; completeRegistration, the first, is missing", bound)
	}
	if len(plain) < 10 {
		t.Errorf("only %d handlers without the key were checked; the walk no longer reaches the handlers", len(plain))
	}
}

// idempotencyPath is the import path of the Idempotency-Key store.
const idempotencyPath = modulePath + "/internal/platform/idempotency"

// keyedCodes are the store's problem codes: universal to the operations
// that take the Idempotency-Key, and only to them (ADR-0034 Karar 7).
// TestHandlersReturnTheirDeclaredErrorCodes adds them to the universal
// codes of exactly those operations.
func keyedCodes() map[string]string {
	out := map[string]string{}
	for _, c := range idempotency.Codes() {
		out[string(c)] = "the Idempotency-Key store answers it for an operation that takes the key"
	}
	return out
}

// TestIdempotencyCodesStayInTheStore: only the store returns its codes. A
// domain package that named them would answer 409 IDEMPOTENCY_KEY_IN_USE
// or 422 IDEMPOTENCY_KEY_REUSED for something else, and a client would
// retry the wrong way; the error binding would not see it, since the
// codes are universal to keyed operations (ADR-0034 Karar 7).
func TestIdempotencyCodesStayInTheStore(t *testing.T) {
	t.Parallel()

	// The store's constants, and the generated client constants of the
	// same codes (apiv1.ErrorCode, which converts to an apperr.Code).
	names := map[string]map[string]bool{
		idempotencyPath: {"CodeIdempotencyKeyReused": true, "CodeIdempotencyKeyInUse": true},
		apiv1Path:       {"ErrorCodeIDEMPOTENCYKEYREUSED": true, "ErrorCodeIDEMPOTENCYKEYINUSE": true},
	}
	values := map[string]bool{}
	for _, c := range idempotency.Codes() {
		values[strconv.Quote(string(c))] = true
	}
	for path, set := range names {
		if len(set) != len(values) {
			t.Fatalf("idempotency.Codes() lists %d codes, this check names %d of %s; name the new one here", len(values), len(set), path)
		}
	}
	importers := map[string]int{}
	goFiles(t, func(fset *token.FileSet, path string, f *ast.File) {
		if strings.Contains(filepath.ToSlash(path), "internal/platform/idempotency/") {
			return
		}
		locals := map[string]string{} // local name → import path
		for pkg := range names {
			if local := importName(f, pkg); local != "" {
				locals[local] = pkg
				importers[pkg]++
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				id, ok := x.X.(*ast.Ident)
				if !ok {
					return true
				}
				if pkg, imported := locals[id.Name]; imported && names[pkg][x.Sel.Name] {
					t.Errorf("%s: %s.%s is the Idempotency-Key store's code; only the store returns it",
						fset.Position(x.Pos()), id.Name, x.Sel.Name)
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING && values[x.Value] {
					t.Errorf("%s: %s is the Idempotency-Key store's code; only the store returns it", fset.Position(x.Pos()), x.Value)
				}
			}
			return true
		})
	})
	// The walk saw the packages that use the store (authn, app) and the
	// handlers that use the generated package.
	if importers[idempotencyPath] < 2 || importers[apiv1Path] < 3 {
		t.Errorf("files importing the store: %d, the generated package: %d; the walk no longer sees them",
			importers[idempotencyPath], importers[apiv1Path])
	}
}
