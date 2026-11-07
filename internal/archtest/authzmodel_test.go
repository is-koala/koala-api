package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The authorization model is loaded with the two recursion rules of
// ADR-0028 applied. loadModel is the same loader with those rules
// relaxed for named object types; it exists for the conformance runner's
// folder model, which is built out of the two cases the rules refuse,
// and it is reached through export_test.go, so it is test-only. Nothing
// but LoadModel may name it in code that ships.
const (
	relaxedLoader = "loadModel"
	loaderGate    = "LoadModel"
)

// relaxedLoaderUses lists every mention of loadModel in f outside
// LoadModel and loadModel themselves, as "file:line in <function>".
// References count, not only calls: `load := loadModel` hands the
// relaxation to whoever gets the value.
func relaxedLoaderUses(fset *token.FileSet, f *ast.File) (found []string, declares bool) {
	mentions := func(n ast.Node, where string) {
		ast.Inspect(n, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == relaxedLoader {
				found = append(found, fset.Position(id.Pos()).String()+" in "+where)
			}
			return true
		})
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			// A package-level var or const initializer can call it too.
			mentions(decl, "a package-level declaration")
			continue
		}
		if fd.Name.Name == relaxedLoader {
			declares = true
			continue // its own declaration and body are the loader itself
		}
		if fd.Name.Name == loaderGate || fd.Body == nil {
			continue
		}
		mentions(fd.Body, fd.Name.Name)
	}
	return found, declares
}

// TestRelaxedModelLoaderStaysOutOfProduction: the recursion rules of
// ADR-0028 are fail-closed, and a loader that turns them off for a type
// would be a way around them that no other test can see. The walk covers
// the module's non-test files; export_test.go is a test file, which is
// the whole point of putting the relaxation there.
func TestRelaxedModelLoaderStaysOutOfProduction(t *testing.T) {
	t.Parallel()
	declared := false
	goFiles(t, func(fset *token.FileSet, _ string, f *ast.File) {
		uses, declares := relaxedLoaderUses(fset, f)
		declared = declared || declares
		for _, where := range uses {
			t.Errorf("%s: %s relaxes the recursion rules of ADR-0028; production code loads the model with %s",
				where, relaxedLoader, loaderGate)
		}
	})
	if !declared {
		t.Errorf("no declaration of %s found; the loader was renamed and this rule now guards nothing", relaxedLoader)
	}
}

// TestRelaxedModelLoaderDetector is the detector's own red and green.
func TestRelaxedModelLoaderDetector(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  string
		want int
	}{
		{
			name: "the gate itself and the loader itself",
			src: `package authz

func LoadModel(data []byte) (*Model, error) { return loadModel(data, nil) }

func loadModel(data []byte, alsoBounded []string) (*Model, error) { return nil, nil }
`,
			want: 0,
		},
		{
			name: "another function calls it with a relaxation",
			src: `package authz

func DefaultModel() (*Model, error) { return loadModel(modelJSON, []string{"folder"}) }
`,
			want: 1,
		},
		{
			name: "taken as a function value",
			src: `package authz

var load = loadModel

func wire() func([]byte, []string) (*Model, error) { return loadModel }
`,
			want: 2,
		},
		{
			name: "a method body",
			src: `package authz

func (s *Service) reload(data []byte) { _, _ = loadModel(data, []string{"job"}) }
`,
			want: 1,
		},
		{
			name: "a file that does not mention it",
			src: `package authz

func DefaultModel() (*Model, error) { return LoadModel(modelJSON) }
`,
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "model.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := relaxedLoaderUses(fset, f)
			if len(got) != tt.want {
				t.Errorf("detected %d uses %v, want %d", len(got), got, tt.want)
			}
			for _, where := range got {
				if strings.Contains(where, " in "+loaderGate) {
					t.Errorf("%s: the gate itself must not be reported", where)
				}
			}
		})
	}
	// The first case declares the loader, the others do not.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "model.go", tests[0].src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if _, declares := relaxedLoaderUses(fset, f); !declares {
		t.Error("the declaration of the loader was not recognized")
	}
	f, err = parser.ParseFile(fset, "model.go", tests[1].src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if _, declares := relaxedLoaderUses(fset, f); declares {
		t.Error("a file that only calls the loader must not count as declaring it")
	}
}
