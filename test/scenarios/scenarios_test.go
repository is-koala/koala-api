// Package scenarios_test holds the CI rule of the scenario catalog
// (README.md "CI kuralı", plan §6.2-1): every catalog file is well formed,
// and once a domain's phase has started every P0 scenario is bound to Go
// tests that exist and carry its id.
package scenarios_test

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// activeDomains are the domains whose phase has started: their P0
// scenarios must each be bound to a Go test or deferred with a reason. A
// domain joins this list in the PR that starts its phase.
func activeDomains() []string {
	return []string{"auth"} // Faz 1
}

// repoRoot is the repository root relative to this directory.
const repoRoot = "../.."

type catalog struct {
	Domain    string     `yaml:"domain"`
	IDPrefix  string     `yaml:"id_prefix"`
	Version   int        `yaml:"version"`
	Scenarios []scenario `yaml:"scenarios"`
}

type scenario struct {
	ID       string   `yaml:"id"`
	Priority string   `yaml:"priority"`
	Title    string   `yaml:"title"`
	Rule     string   `yaml:"rule"`
	Given    []string `yaml:"given"`
	When     []string `yaml:"when"`
	Then     []string `yaml:"then"`
	Error    string   `yaml:"error"`
	Decision string   `yaml:"decision"`
	Target   string   `yaml:"target"`
	Oracle   string   `yaml:"oracle"`
	Source   []string `yaml:"source"`
	// GoTest is null, one test name or a list of them:
	// "internal/authn.TestRefreshRotation" or ".../TestX/sub".
	GoTest yaml.Node `yaml:"go_test"`
	// Deferred names the phase a P0 of an active domain waits for and why.
	Deferred string `yaml:"deferred"`
	// Partial says which part of the rule the bound tests leave to a later
	// PR or phase.
	Partial string `yaml:"partial"`
}

func (s scenario) goTests() ([]string, error) {
	switch s.GoTest.Kind {
	case 0:
		return nil, nil
	case yaml.ScalarNode:
		if s.GoTest.Tag == "!!null" {
			return nil, nil
		}
		return []string{s.GoTest.Value}, nil
	case yaml.SequenceNode:
		var out []string
		if err := s.GoTest.Decode(&out); err != nil {
			return nil, fmt.Errorf("go_test: %w", err)
		}
		return out, nil
	case yaml.DocumentNode, yaml.MappingNode, yaml.AliasNode:
		return nil, errors.New("go_test: want null, a test name or a list of test names")
	}
	return nil, errors.New("go_test: unknown YAML node")
}

func loadCatalogs(t *testing.T) map[string]catalog {
	t.Helper()
	files, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no scenario files")
	}
	out := map[string]catalog{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		var c catalog
		if err := dec.Decode(&c); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[f] = c
	}
	return out
}

var idPattern = regexp.MustCompile(`^([A-Z]+)-\d{3}$`)

// Rules 1 and 2: ids are unique and carry the file's prefix; every
// scenario has a priority and a source.
func TestCatalogsAreWellFormed(t *testing.T) {
	t.Parallel()

	seen := map[string]string{}
	for file, c := range loadCatalogs(t) {
		if c.Domain == "" || c.IDPrefix == "" {
			t.Errorf("%s: domain and id_prefix are required", file)
		}
		for _, s := range c.Scenarios {
			m := idPattern.FindStringSubmatch(s.ID)
			if len(m) != 2 || m[1] != c.IDPrefix {
				t.Errorf("%s: id %q is not %s-nnn", file, s.ID, c.IDPrefix)
			}
			if prev, ok := seen[s.ID]; ok {
				t.Errorf("%s: id %s already used in %s", file, s.ID, prev)
			}
			seen[s.ID] = file
			if !slices.Contains([]string{"P0", "P1", "P2"}, s.Priority) {
				t.Errorf("%s: %s priority %q", file, s.ID, s.Priority)
			}
			if len(s.Source) == 0 {
				t.Errorf("%s: %s has no source", file, s.ID)
			}
			if _, err := s.goTests(); err != nil {
				t.Errorf("%s: %s: %v", file, s.ID, err)
			}
		}
	}
}

// Rule 3: in an active domain every P0 is bound to Go tests or deferred
// with a reason, never both. Every bound test, of any priority, exists in
// its package's test files and carries the scenario id in its doc comment
// or body. That it passes is the gate's job: make verify runs the unit
// tests, make test-integration the integration ones.
func TestScenariosAreBoundToTests(t *testing.T) {
	t.Parallel()

	tests := testIndex{}
	for file, c := range loadCatalogs(t) {
		active := slices.Contains(activeDomains(), c.Domain)
		for _, s := range c.Scenarios {
			bound, err := s.goTests()
			if err != nil {
				continue // reported by TestCatalogsAreWellFormed
			}
			if active && s.Priority == "P0" {
				switch {
				case len(bound) == 0 && strings.TrimSpace(s.Deferred) == "":
					t.Errorf("%s: P0 %s has neither go_test nor deferred", file, s.ID)
				case len(bound) > 0 && s.Deferred != "":
					t.Errorf("%s: %s has both go_test and deferred", file, s.ID)
				}
			}
			for _, name := range bound {
				if err := tests.check(name, s.ID); err != nil {
					t.Errorf("%s: %s: %v", file, s.ID, err)
				}
			}
		}
	}
}

// testIndex caches the parsed test files of each package directory.
type testIndex map[string]map[string]string // dir → Test function → its source

func (ix testIndex) check(name, id string) error {
	pkg, test, ok := strings.Cut(name, ".")
	if !ok || !strings.HasPrefix(pkg, "internal/") {
		return fmt.Errorf("go_test %q: want internal/<package>.TestName[/subtest]", name)
	}
	fn, _, _ := strings.Cut(test, "/")
	funcs, err := ix.load(pkg)
	if err != nil {
		return err
	}
	src, ok := funcs[fn]
	if !ok {
		return fmt.Errorf("go_test %q: %s has no test function %s", name, pkg, fn)
	}
	if !strings.Contains(src, id) {
		return fmt.Errorf("go_test %q: %s does not mention %s in its doc comment or body", name, fn, id)
	}
	return nil
}

func (ix testIndex) load(pkg string) (map[string]string, error) {
	if funcs, ok := ix[pkg]; ok {
		return funcs, nil
	}
	files, err := filepath.Glob(filepath.Join(repoRoot, pkg, "*_test.go"))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", pkg, err)
	}
	funcs := map[string]string{}
	fset := token.NewFileSet()
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		file, err := parser.ParseFile(fset, f, src, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "Test") {
				continue
			}
			start := fd.Pos()
			if fd.Doc != nil {
				start = fd.Doc.Pos()
			}
			funcs[fd.Name.Name] = string(src[fset.Position(start).Offset:fset.Position(fd.End()).Offset])
		}
	}
	ix[pkg] = funcs
	return funcs, nil
}
