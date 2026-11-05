package authn_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecretsComparedInConstantTime: the one place this package compares a
// secret in Go is the argon2id key, and it does so with
// subtle.ConstantTimeCompare. Refresh tokens and tickets are looked up by
// their SHA-256 in the database, never compared here. No production file
// may reach for a variable-time byte comparison (bytes.Equal,
// reflect.DeepEqual) instead.
func TestSecretsComparedInConstantTime(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	constantTime := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch pkg.Name + "." + sel.Sel.Name {
			case "bytes.Equal", "bytes.Compare", "reflect.DeepEqual":
				t.Errorf("%s: %s.%s compares in variable time; use subtle.ConstantTimeCompare", fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name)
			case "subtle.ConstantTimeCompare":
				if name == "password.go" {
					constantTime++
				}
			}
			return true
		})
	}
	if constantTime == 0 {
		t.Error("password.go does not compare the derived key with subtle.ConstantTimeCompare")
	}
}
