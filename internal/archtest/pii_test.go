package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// mentionsValue reports whether the type expression names <name>.Value
// (Value alone under a dot import) at any depth, so pii.Value[T],
// []pii.Value[T], *pii.Value[T] and map[K]pii.Value[T] all count.
func mentionsValue(t ast.Expr, name string) bool {
	mentions := false
	ast.Inspect(t, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && name != "." && id.Name == name && x.Sel.Name == "Value" {
				mentions = true
			}
		case *ast.Ident:
			if name == "." && x.Name == "Value" {
				mentions = true
			}
		}
		return !mentions
	})
	return mentions
}

// valuesInUnexportedFields lists every unexported struct field of f whose
// type holds a pii.Value. Aliased and dot imports count like a plain one.
func valuesInUnexportedFields(fset *token.FileSet, f *ast.File) []string {
	name := piiImport(f)
	if name == "" {
		return nil
	}
	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for _, field := range st.Fields.List {
			if !mentionsValue(field.Type, name) {
				continue
			}
			for _, id := range field.Names {
				if !id.IsExported() {
					found = append(found, fset.Position(id.Pos()).String()+" "+id.Name)
				}
			}
		}
		return true
	})
	slices.Sort(found)
	return found
}

// TestPIIValuesStayInExportedFields: a pii.Value is carried in an
// exported struct field, never an unexported one (ADR-0026).
//
// pii.Value masks itself through String, GoString, Format, LogValue and
// MarshalJSON, but fmt reaches those methods only where CanInterface
// holds. A reflect.Value read out of an unexported field carries flagRO,
// so fmt cannot call the methods and prints the struct's insides instead
// - the clear value - exactly as time.Time shows up as {wall ext loc}.
// Go cannot close this from inside the type, so the rule is here:
// `type row struct { phone pii.Value[string] }` plus slog.Any("row", r)
// or %+v would put a phone number in the log.
func TestPIIValuesStayInExportedFields(t *testing.T) {
	t.Parallel()
	goFilesIn(t, []string{moduleRoot}, func(fset *token.FileSet, _ string, f *ast.File) {
		if f.Name.Name == "pii" {
			return
		}
		for _, where := range valuesInUnexportedFields(fset, f) {
			t.Errorf("%s: a pii.Value in an unexported field; fmt reflects into it and prints the value in clear, so the field must be exported", where)
		}
	})
}

// TestPIIValueFieldDetector keeps the rule above from passing vacuously
// while no struct carries a pii.Value yet, and proves what it catches.
func TestPIIValueFieldDetector(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		src  string
		want []string // suffixes of the findings
	}{
		"exported and unexported": {`package user

import "github.com/is-koala/koala-api/internal/pii"

type Row struct {
	Phone pii.Value[string]
	tckn  pii.Value[string]
}
`, []string{" tckn"}},
		"wrapped in a slice, a pointer and a map": {`package user

import "github.com/is-koala/koala-api/internal/pii"

type row struct {
	phones  []pii.Value[string]
	primary *pii.Value[string]
	byField map[pii.Field]pii.Value[string]
}
`, []string{" phones", " primary", " byField"}},
		// The field that holds the nested struct is a finding of its own:
		// fmt reflecting into it reaches the Value the same way.
		"aliased import, a local struct and a nested one": {`package user

import p "github.com/is-koala/koala-api/internal/pii"

func f() {
	var local struct {
		phone p.Value[string]
		inner struct {
			tckn p.Value[string]
		}
	}
	_ = local
}
`, []string{" phone", " inner", " tckn"}},
		"no pii.Value": {`package user

import "github.com/is-koala/koala-api/internal/pii"

type row struct {
	grant *pii.Grant
	note  string
}
`, nil},
		"not imported": {`package user

type row struct {
	phone Value[string]
}
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
			got := valuesInUnexportedFields(fset, f)
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
