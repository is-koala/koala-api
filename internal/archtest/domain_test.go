// Package archtest holds repository-wide architecture tests: rules that
// golangci-lint cannot express.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// jsonTaggedFields returns "file:line Type.Field" for every struct field in
// the parsed file that carries a json struct tag.
func jsonTaggedFields(fset *token.FileSet, f *ast.File) []string {
	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for _, field := range st.Fields.List {
			if field.Tag == nil {
				continue
			}
			tag, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				continue
			}
			if _, ok := reflect.StructTag(tag).Lookup("json"); ok {
				found = append(found, fset.Position(field.Pos()).String())
			}
		}
		return true
	})
	return found
}

// TestDomainFilesCarryNoJSONTags enforces plan §3.2-1: domain.go holds
// types, rules and state machines; wire formats live in http.go. depguard
// already keeps encoding/json out of domain.go, but a json tag needs no
// import, so it is checked here. The walk covers the module, not only
// internal/, so a domain.go in a package added later is in scope.
func TestDomainFilesCarryNoJSONTags(t *testing.T) {
	t.Parallel()

	seen := 0
	goFiles(t, func(fset *token.FileSet, path string, f *ast.File) {
		if filepath.Base(path) != "domain.go" {
			return
		}
		seen++
		for _, pos := range jsonTaggedFields(fset, f) {
			t.Errorf("%s: domain.go must not carry json struct tags; map to a DTO in http.go", pos)
		}
	})
	if seen == 0 {
		t.Error("no domain.go found; the walk no longer reaches the domain packages")
	}
}

// TestJSONTagDetector keeps the rule above from passing vacuously while no
// domain.go exists yet.
func TestJSONTagDetector(t *testing.T) {
	t.Parallel()

	const src = `package job

type Job struct {
	ID     string
	Title  string ` + "`json:\"title\"`" + `
	Status string ` + "`db:\"status\"`" + `
	Nested struct {
		Note string ` + "`json:\"note,omitempty\" db:\"note\"`" + `
	}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "domain.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if got := jsonTaggedFields(fset, f); len(got) != 2 {
		t.Fatalf("detected %v, want the Title and Nested.Note fields", got)
	}
}
