package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// staffContextHelper is internal/authn's impersonation primitive: it puts
// a staff principal in a context without authenticating it, for a test
// that drives the staff gate without the middleware. The staff gate
// grants the principal it finds that member's real permissions and audits
// in its name, so production code must never name it. Go has no friend
// packages: a depguard rule fences an import, not a function, so the
// fence is here and it holds in every package of the module: the walk
// starts at the repository root, so a new top-level package is in scope
// the day it appears.
const staffContextHelper = "ContextWithStaffForTest"

// impersonations returns the positions of every mention of
// staffContextHelper in f other than its own declaration — a call, a
// function value, an alias — and reports whether f declares it. The check
// is by name, so an aliased or a dot import is caught like a plain one.
func impersonations(fset *token.FileSet, f *ast.File) (found []string, declares bool) {
	decl := map[*ast.Ident]bool{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != staffContextHelper {
			continue
		}
		decl[fd.Name] = true
		declares = true
	}
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if ok && id.Name == staffContextHelper && !decl[id] {
			found = append(found, fset.Position(id.Pos()).String())
		}
		return true
	})
	slices.Sort(found)
	return found, declares
}

// TestNoStaffImpersonationOutsideTests enforces the fence of ADR-0026: a
// _test.go file may hand itself a staff principal, no other file may. A
// handler or a River job that wrote
// authn.ContextWithStaffForTest(ctx, authn.StaffPrincipal{StaffID: id})
// would act with that member's permissions — a disabled, soft-deleted or
// revoked member's included — past authn's token version, status and
// soft-delete checks, and the audit row would carry their name.
func TestNoStaffImpersonationOutsideTests(t *testing.T) {
	t.Parallel()
	declared := 0
	goFilesIn(t, []string{moduleRoot}, func(fset *token.FileSet, _ string, f *ast.File) {
		found, declares := impersonations(fset, f)
		if declares {
			declared++
		}
		for _, where := range found {
			t.Errorf("%s: %s is a test-only impersonation primitive; only a _test.go file may name it", where, staffContextHelper)
		}
	})
	// Without this the rule would pass vacuously once the helper is
	// renamed or removed.
	if declared != 1 {
		t.Errorf("%d files declare %s, want the one in internal/authn", declared, staffContextHelper)
	}
}

// TestImpersonationDetector proves the rule above catches the ways a
// production file could reach the helper, and leaves its declaration
// alone.
func TestImpersonationDetector(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		src      string
		want     []string // "file:line:" prefixes of the findings
		declares bool
	}{
		"the declaration itself": {`package authn

func ContextWithStaffForTest(ctx context.Context, p StaffPrincipal) context.Context {
	return context.WithValue(ctx, staffKey{}, p)
}
`, nil, true},
		"a call in a handler": {`package admin

import "github.com/is-koala/koala-api/internal/authn"

func serve(w http.ResponseWriter, r *http.Request) {
	ctx := authn.ContextWithStaffForTest(r.Context(), authn.StaffPrincipal{StaffID: owner})
	_ = ctx
}
`, []string{"handler.go:6:"}, false},
		"an aliased import and a function value": {`package job

import a "github.com/is-koala/koala-api/internal/authn"

var impersonate = a.ContextWithStaffForTest
`, []string{"handler.go:5:"}, false},
		"a dot import": {`package job

import . "github.com/is-koala/koala-api/internal/authn"

func run(ctx context.Context) context.Context { return ContextWithStaffForTest(ctx, StaffPrincipal{}) }
`, []string{"handler.go:5:"}, false},
		"an unrelated file": {`package job

func run(ctx context.Context) context.Context { return ctx }
`, nil, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "handler.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			got, declares := impersonations(fset, f)
			if declares != tt.declares {
				t.Errorf("declares = %v, want %v", declares, tt.declares)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("detected %v, want %d findings ending in %v", got, len(tt.want), tt.want)
			}
			for i, w := range tt.want {
				if !strings.HasPrefix(got[i], w) {
					t.Errorf("finding %q, want one starting with %q", got[i], w)
				}
			}
		})
	}
}
