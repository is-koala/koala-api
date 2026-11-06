package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// goFiles parses every non-test, non-generated Go file under internal/
// and calls fn with it.
func goFiles(t *testing.T, fn func(fset *token.FileSet, path string, f *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			strings.HasSuffix(path, ".gen.go") || strings.HasSuffix(path, ".sql.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		fn(fset, path, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// callsIn returns the positions of method calls x.name(…) inside the
// function declarations of f, by enclosing function.
func callsIn(f *ast.File, name string) map[*ast.FuncDecl][]token.Pos {
	out := map[*ast.FuncDecl][]token.Pos{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
				out[fd] = append(out[fd], call.Pos())
			}
			return true
		})
	}
	return out
}

// piiPath is the import path of the PII gate.
const piiPath = "github.com/is-koala/koala-api/internal/pii"

// piiImport returns the name f uses for the pii package: its alias, "pii",
// or "." for a dot import; "" when f does not import it.
func piiImport(f *ast.File) string {
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != piiPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "pii"
	}
	return ""
}

// takesGrant reports whether the function type has a parameter of type
// *<name>.Grant (*Grant under a dot import).
func takesGrant(ft *ast.FuncType, name string) bool {
	if ft == nil || ft.Params == nil {
		return false
	}
	for _, p := range ft.Params.List {
		star, ok := p.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		switch x := star.X.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == name && x.Sel.Name == "Grant" {
				return true
			}
		case *ast.Ident:
			if name == "." && x.Name == "Grant" {
				return true
			}
		}
	}
	return false
}

// revealsOutsideProjections lists every use of pii.Reveal that is not a
// direct call in a function with a *pii.Grant parameter: a call in a
// function (or func literal, package-level ones included) without the
// grant, or Reveal taken as a value (a method or function value that can
// be called anywhere). Aliased and dot imports count like a plain one.
func revealsOutsideProjections(fset *token.FileSet, f *ast.File) []string {
	name := piiImport(f)
	if name == "" {
		return nil
	}
	var found []string
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		parent := ast.Node(nil)
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		stack = append(stack, n)
		if !isReveal(n, parent, name) {
			return true
		}
		where := fset.Position(n.Pos()).String()
		if !calledDirectly(stack) {
			found = append(found, where+" Reveal used as a value")
			return true
		}
		fn, fnName := enclosingFunc(stack)
		if !takesGrant(fn, name) {
			found = append(found, where+" in "+fnName)
		}
		return true
	})
	slices.Sort(found)
	return found
}

// isReveal reports whether n names pii.Reveal: <name>.Reveal, or Reveal
// itself under a dot import (not the selector of another value).
func isReveal(n, parent ast.Node, name string) bool {
	switch x := n.(type) {
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && name != "." && id.Name == name && x.Sel.Name == "Reveal"
	case *ast.Ident:
		if name != "." || x.Name != "Reveal" {
			return false
		}
		if sel, ok := parent.(*ast.SelectorExpr); ok && sel.Sel == x {
			return false
		}
		if fd, ok := parent.(*ast.FuncDecl); ok && fd.Name == x {
			return false
		}
		return true
	}
	return false
}

// calledDirectly reports whether the last node of stack is the function
// of a call: Reveal(…) or Reveal[T](…).
func calledDirectly(stack []ast.Node) bool {
	ref := stack[len(stack)-1]
	i := len(stack) - 2
	if i >= 0 {
		switch ix := stack[i].(type) {
		case *ast.IndexExpr:
			if ix.X == ref {
				ref, i = ix, i-1
			}
		case *ast.IndexListExpr:
			if ix.X == ref {
				ref, i = ix, i-1
			}
		}
	}
	if i < 0 {
		return false
	}
	call, ok := stack[i].(*ast.CallExpr)
	return ok && call.Fun == ref
}

// enclosingFunc returns the type and a name of the innermost function
// around the last node of stack: a declaration or a func literal; nil at
// package level outside any function.
func enclosingFunc(stack []ast.Node) (*ast.FuncType, string) {
	for i := len(stack) - 1; i >= 0; i-- {
		switch fn := stack[i].(type) {
		case *ast.FuncLit:
			return fn.Type, "a func literal"
		case *ast.FuncDecl:
			return fn.Type, fn.Name.Name
		}
	}
	return nil, "package level"
}

// TestRevealOnlyInProjections enforces the PII gate's rule (plan §5.5,
// ADR-0026): a personal value leaves in clear only through pii.Reveal, and
// pii.Reveal is called only from a projection that takes the *pii.Grant as
// a parameter, so the grant Gate.Open made (or nil, masked) is visible in
// the projection's signature. The pii package itself is exempt.
func TestRevealOnlyInProjections(t *testing.T) {
	t.Parallel()
	goFiles(t, func(fset *token.FileSet, _ string, f *ast.File) {
		if f.Name.Name == "pii" {
			return
		}
		for _, where := range revealsOutsideProjections(fset, f) {
			t.Errorf("%s: pii.Reveal in a function without a *pii.Grant parameter; write a projection that takes the grant", where)
		}
	})
}

// TestRevealDetector keeps the rule above from passing vacuously while no
// projection exists yet, and proves the ways around it are caught.
func TestRevealDetector(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		src  string
		want []string // suffixes of the findings
	}{
		"projection and leak": {`package user

import "github.com/is-koala/koala-api/internal/pii"

func ToAdminDTO(u User, g *pii.Grant) DTO {
	phone, _ := pii.Reveal(g, u.Phone)
	return DTO{Phone: phone}
}

func leak(u User) string {
	phone, _ := pii.Reveal[string](nil, u.Phone)
	return phone
}
`, []string{" in leak"}},
		"aliased import": {`package user

import p "github.com/is-koala/koala-api/internal/pii"

func ok(u User, g *p.Grant) string { v, _ := p.Reveal(g, u.Phone); return v }

func leak(u User) string { v, _ := p.Reveal(nil, u.Phone); return v }
`, []string{" in leak"}},
		"dot import": {`package user

import . "github.com/is-koala/koala-api/internal/pii"

func ok(u User, g *Grant) string { v, _ := Reveal(g, u.Phone); return v }

func leak(u User) string { v, _ := Reveal(nil, u.Phone); return v }
`, []string{" in leak"}},
		"package-level func literal": {`package user

import "github.com/is-koala/koala-api/internal/pii"

var leak = func(u User) string { v, _ := pii.Reveal(nil, u.Phone); return v }
`, []string{" in a func literal"}},
		"function value": {`package user

import "github.com/is-koala/koala-api/internal/pii"

var reveal = pii.Reveal[string]

func ok(u User, g *pii.Grant) func(*pii.Grant, pii.Value[string]) (string, bool) { return pii.Reveal[string] }
`, []string{" Reveal used as a value", " Reveal used as a value"}},
		"not imported": {`package user

func Reveal() {}

func f() { Reveal() }
`, nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "user.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			got := revealsOutsideProjections(fset, f)
			if len(got) != len(tt.want) {
				t.Fatalf("detected %v, want %d findings ending in %v", got, len(tt.want), tt.want)
			}
			for i, w := range tt.want {
				if !strings.HasSuffix(got[i], w) {
					t.Errorf("finding %q, want one ending in %q", got[i], w)
				}
			}
		})
	}
}

// relationWriters are the generated queries that write a relationship
// table (staff_role_assignments). ADR-0024 §10: they are called only from
// store.go, by functions that emit the change to the RelationSink in the
// same transaction (a call of Record).
func relationWriters() []string { return []string{"AssignStaffRole"} }

// relationWritesWithoutChange lists calls of a relation writer outside
// store.go or in a function that does not call Record.
func relationWritesWithoutChange(fset *token.FileSet, path string, f *ast.File) []string {
	var found []string
	for _, w := range relationWriters() {
		for fd, calls := range callsIn(f, w) {
			records := len(callsIn(&ast.File{Decls: []ast.Decl{fd}}, "Record")) > 0
			if filepath.Base(path) == "store.go" && records {
				continue
			}
			for _, pos := range calls {
				found = append(found, fset.Position(pos).String()+" "+w+" in "+fd.Name.Name)
			}
		}
	}
	slices.Sort(found)
	return found
}

// TestRelationWritesEmitChanges: every write to a relationship table emits
// its authz.RelationChange in the same function (plan §5.2-5, ADR-0024
// §10, ADR-0026); depguard keeps the queries in store.go.
func TestRelationWritesEmitChanges(t *testing.T) {
	t.Parallel()
	seen := 0
	goFiles(t, func(fset *token.FileSet, path string, f *ast.File) {
		for _, w := range relationWriters() {
			for _, calls := range callsIn(f, w) {
				seen += len(calls)
			}
		}
		for _, where := range relationWritesWithoutChange(fset, path, f) {
			t.Errorf("%s: a relationship write must sit in store.go in a function that records its authz.RelationChange", where)
		}
	})
	if seen == 0 {
		t.Error("no relationship write found; the writer list is stale")
	}
}

// TestRelationWriteDetector proves the rule above catches a write without
// its change.
func TestRelationWriteDetector(t *testing.T) {
	t.Parallel()
	const src = `package staff

func (s store) good(ctx context.Context, q db.Querier) error {
	_ = maintdb.New(q).AssignStaffRole(ctx, p)
	return s.sink.Record(ctx, q, ch)
}

func (s store) bad(ctx context.Context, q db.Querier) error {
	return maintdb.New(q).AssignStaffRole(ctx, p)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "store.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got := relationWritesWithoutChange(fset, "internal/staff/store.go", f)
	if len(got) != 1 || !strings.HasSuffix(got[0], " in bad") {
		t.Fatalf("detected %v, want the write in bad only", got)
	}
}
