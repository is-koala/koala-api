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

// callsIn returns the positions of calls to pkg.name (pkg "" for a call
// on any receiver, x.name) inside the function declarations of f, each
// with its enclosing function.
func callsIn(f *ast.File, pkg, name string) map[*ast.FuncDecl][]token.Pos {
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
			fun := call.Fun
			if ix, ok := fun.(*ast.IndexExpr); ok { // pii.Reveal[string](…)
				fun = ix.X
			}
			sel, ok := fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != name {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); pkg == "" || (ok && id.Name == pkg) {
				out[fd] = append(out[fd], call.Pos())
			}
			return true
		})
	}
	return out
}

// takesGrant reports whether fd has a parameter of type *pii.Grant.
func takesGrant(fd *ast.FuncDecl) bool {
	for _, p := range fd.Type.Params.List {
		star, ok := p.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "pii" && sel.Sel.Name == "Grant" {
			return true
		}
	}
	return false
}

// revealsOutsideProjections lists the calls of pii.Reveal in functions
// without a *pii.Grant parameter.
func revealsOutsideProjections(fset *token.FileSet, f *ast.File) []string {
	var found []string
	for fd, calls := range callsIn(f, "pii", "Reveal") {
		if takesGrant(fd) {
			continue
		}
		for _, pos := range calls {
			found = append(found, fset.Position(pos).String()+" in "+fd.Name.Name)
		}
	}
	slices.Sort(found)
	return found
}

// TestRevealOnlyInProjections enforces the PII gate's rule (plan §5.5,
// ADR-0026): a personal value leaves in clear only through pii.Reveal, and
// pii.Reveal is called only from a projection that takes the *pii.Grant as
// a parameter, so the grant Gate.Open made (or nil, masked) is visible in
// the projection's signature. The pii package itself is exempt.
func TestRevealOnlyInProjections(t *testing.T) {
	t.Parallel()
	goFiles(t, func(fset *token.FileSet, path string, f *ast.File) {
		if f.Name.Name == "pii" {
			return
		}
		for _, where := range revealsOutsideProjections(fset, f) {
			t.Errorf("%s: pii.Reveal in a function without a *pii.Grant parameter; write a projection that takes the grant", where)
		}
	})
}

// TestRevealDetector keeps the rule above from passing vacuously while no
// projection exists yet.
func TestRevealDetector(t *testing.T) {
	t.Parallel()
	const src = `package user

func ToAdminDTO(u User, g *pii.Grant) DTO {
	phone, _ := pii.Reveal(g, u.Phone)
	return DTO{Phone: phone}
}

func leak(u User) string {
	phone, _ := pii.Reveal[string](nil, u.Phone)
	return phone
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "user.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got := revealsOutsideProjections(fset, f)
	if len(got) != 1 || !strings.HasSuffix(got[0], " in leak") {
		t.Fatalf("detected %v, want the call in leak only", got)
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
		for fd, calls := range callsIn(f, "", w) {
			records := len(callsIn(&ast.File{Decls: []ast.Decl{fd}}, "", "Record")) > 0
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
			for _, calls := range callsIn(f, "", w) {
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
