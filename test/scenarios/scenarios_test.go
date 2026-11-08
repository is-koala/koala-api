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
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// activeDomains are the domains whose phase has started: their P0
// scenarios must each be bound to a Go test or deferred with a reason. A
// domain joins this list in the PR that starts its phase.
func activeDomains() []string {
	return []string{
		"auth",                                 // Faz 1
		"user", "identity", "company", "media", // Faz 2 (ADR-0029)
	}
}

// frozenDomains maps a catalog domain to the contract tags that carry its
// operations (ADR-0029 §1). TestFrozenDomainsDeferTheirScenarios holds the
// contract and the catalog together through it: while a tag still has an
// operation marked x-not-implemented the domain's P0 scenarios must be
// deferred, and once no operation of the tag is marked they must be bound.
// The granularity is the domain, not the scenario: a scenario is written
// in API-independent language and names no operation (README.md), so a
// per-scenario map would leak the contract into the catalog.
func frozenDomains() map[string][]string {
	return map[string][]string{
		"user":     {"users", "admin-users"},
		"identity": {"identity", "admin-identity"},
		"company":  {"company", "admin-company"},
		"media":    {"media", "address", "reference"},
	}
}

// currentPhase is the plan phase (§6.1) the repository is in. A deferred
// or partial scenario names the phase that closes it; once that phase is
// behind (the PR that exits it raises this constant), the scenario is red
// until it is bound in full (ADR-0025 §11).
const currentPhase = 2

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
	Deferred *pending `yaml:"deferred"`
	// Partial says which part of the rule the bound tests leave to a later
	// PR or phase.
	Partial *pending `yaml:"partial"`
}

// pending is work a scenario still waits for: the plan phase that closes
// it and why.
type pending struct {
	Phase  int    `yaml:"phase"`
	Reason string `yaml:"reason"`
}

func (p *pending) check(field string) error {
	switch {
	case p == nil:
		return nil
	case p.Phase < 1 || strings.TrimSpace(p.Reason) == "":
		return fmt.Errorf("%s needs a phase (≥ 1) and a reason", field)
	case p.Phase < currentPhase:
		return fmt.Errorf("%s waits for phase %d, which is over (current phase %d): bind the scenario in full", field, p.Phase, currentPhase)
	}
	return nil
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
// with a reason, never both, and nothing waits for a phase that is over.
// Every bound test, of any priority, exists in its package's test files
// and carries the scenario id in a string of its body (a subtest name or a
// failure message: a doc comment is not enough); a bound subtest exists as
// a t.Run name. That it passes is the gate's job: make verify runs the
// unit tests, make test-integration the integration ones.
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
				case len(bound) == 0 && s.Deferred == nil:
					t.Errorf("%s: P0 %s has neither go_test nor deferred", file, s.ID)
				case len(bound) > 0 && s.Deferred != nil:
					t.Errorf("%s: %s has both go_test and deferred", file, s.ID)
				}
			}
			for field, p := range map[string]*pending{"deferred": s.Deferred, "partial": s.Partial} {
				if err := p.check(field); err != nil {
					t.Errorf("%s: %s: %v", file, s.ID, err)
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
type testIndex map[string]map[string][]string // dir → Test function → its body's string literals

func (ix testIndex) check(name, id string) error {
	pkg, test, ok := strings.Cut(name, ".")
	if !ok || !strings.HasPrefix(pkg, "internal/") {
		return fmt.Errorf("go_test %q: want internal/<package>.TestName[/subtest]", name)
	}
	fn, sub, hasSub := strings.Cut(test, "/")
	funcs, err := ix.load(pkg)
	if err != nil {
		return err
	}
	strs, ok := funcs[fn]
	if !ok {
		return fmt.Errorf("go_test %q: %s has no test function %s", name, pkg, fn)
	}
	if !slices.ContainsFunc(strs, func(v string) bool { return strings.Contains(v, id) }) {
		return fmt.Errorf("go_test %q: no string in the body of %s carries %s", name, fn, id)
	}
	if hasSub && !slices.ContainsFunc(strs, func(v string) bool { return v == sub || strings.HasPrefix(v, sub+"/") }) {
		return fmt.Errorf("go_test %q: %s has no subtest %q", name, fn, sub)
	}
	return nil
}

func (ix testIndex) load(pkg string) (map[string][]string, error) {
	if funcs, ok := ix[pkg]; ok {
		return funcs, nil
	}
	files, err := filepath.Glob(filepath.Join(repoRoot, pkg, "*_test.go"))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", pkg, err)
	}
	funcs := map[string][]string{}
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
			if fd.Body == nil {
				continue
			}
			var strs []string
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						strs = append(strs, v)
					}
				}
				return true
			})
			funcs[fd.Name.Name] = strs
		}
	}
	ix[pkg] = funcs
	return funcs, nil
}
