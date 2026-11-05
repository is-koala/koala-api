package authz_test

import (
	"context"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/internal/matrix"
)

// basisExceptions are allow rows whose matrix basis is one valid ground
// while the evaluator reports another, equally valid one first.
var basisExceptions = map[string]string{
	"AZM-106": "u_co_admin is both the inviter and a company admin; the union lists inviter first",
	"AZM-160": "the matrix names user#itself; the model grants the CV through private_file#owner",
}

// concealExceptions are deny rows whose caller does not reject, so
// Decision.Conceal does not apply.
var concealExceptions = map[string]string{
	"AZM-055": "field-level projection: the public view of the job",
	"AZM-130": "WebSocket subscription refusal; there is no 404 on a socket",
	"AZM-150": "field-level projection: plate and driver hidden",
	"AZM-162": "field-level projection: no CV link",
}

// TestMatrixGoEvaluator runs every [go, openfga] row of the matrix on the
// Go evaluator: decision, basis and concealment. The same rows run on a
// real OpenFGA server in openfga_conformance_test.go.
func TestMatrixGoEvaluator(t *testing.T) {
	t.Parallel()
	mx := loadMatrix(t)
	ev := newMatrixEvaluator(t, mx)
	ran := 0
	for i := range mx.Cases {
		c := &mx.Cases[i]
		if reason := notRunReason(mx, c); reason != "" {
			t.Logf("%s not run: %s", c.ID, reason)
			continue
		}
		ran++
		t.Run(c.ID, func(t *testing.T) {
			t.Parallel()
			objectType, relation, ok := mx.Relation(c.Action)
			if !ok {
				t.Fatalf("action %s maps to %q, not a relation", c.Action, mx.Actions[c.Action])
			}
			s, ok := fixtureSubject(t, c.Subject)
			if !ok {
				t.Fatalf("anonymous subject in a [go, openfga] row")
			}
			o := fixtureObject(t, c.Object)
			if o.Type != objectType {
				t.Fatalf("object %s is not a %s", c.Object, objectType)
			}
			d, err := ev.Check(context.Background(), s, authz.Action(relation), o)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if d.Allowed != c.Allowed() {
				t.Fatalf("%s %s on %s: allowed %v, want %s", c.Subject, relation, c.Object, d.Allowed, c.Expect)
			}
			if d.Allowed {
				if d.Conceal {
					t.Errorf("allowed decision with Conceal")
				}
				if _, skip := basisExceptions[c.ID]; !skip && !basisHolds(mx, c.Basis, d.Basis) {
					t.Errorf("basis %q does not show the matrix basis %q (%s)", d.Basis, c.Basis, mx.Basis[c.Basis])
				}
				return
			}
			if d.Basis != "" {
				t.Errorf("denied decision with basis %q", d.Basis)
			}
			if _, skip := concealExceptions[c.ID]; !skip && d.Conceal != *c.Conceal {
				t.Errorf("conceal %v, want %v", d.Conceal, *c.Conceal)
			}
		})
	}
	if ran == 0 {
		t.Fatal("no [go, openfga] row ran")
	}
	for _, ids := range []map[string]string{basisExceptions, concealExceptions} {
		for id := range ids {
			if !hasDualCase(mx, id) {
				t.Errorf("exception %s is not a [go, openfga] row of the matrix", id)
			}
		}
	}
}

// basisHolds reports whether the evaluator's basis shows the matrix's
// basis key: the relation the key's description names, a wildcard grant
// for "public", or a relation named like the key ("owner").
func basisHolds(mx *matrix.Matrix, key, basis string) bool {
	hops := strings.Split(basis, " > ")
	if rel, ok := mx.BasisRelation(key); ok {
		for _, h := range hops {
			if hop, _, _ := strings.Cut(h, " "); hop == rel {
				return true
			}
		}
		return false
	}
	if key == "public" {
		return strings.HasSuffix(hops[len(hops)-1], ":*]")
	}
	for _, h := range hops {
		hop, _, _ := strings.Cut(h, " ")
		if _, rel, _ := strings.Cut(hop, "#"); rel == key {
			return true
		}
	}
	return false
}

func hasDualCase(mx *matrix.Matrix, id string) bool {
	for i := range mx.Cases {
		if mx.Cases[i].ID == id {
			return mx.Cases[i].Engine == matrix.EngineBoth
		}
	}
	return false
}

// TestMatrixShape pins the matrix this runner was written against; a
// regenerated matrix with other counts gets a deliberate look.
func TestMatrixShape(t *testing.T) {
	t.Parallel()
	mx := loadMatrix(t)
	counts := map[string]int{}
	for i := range mx.Cases {
		counts[mx.Cases[i].Engine]++
	}
	want := map[string]int{matrix.EngineBoth: 152, matrix.EngineGoOnly: 55, matrix.EngineSkip: 6}
	for engine, n := range want {
		if counts[engine] != n {
			t.Errorf("%s rows: %d, want %d", engine, counts[engine], n)
		}
	}
	if len(mx.Cases) != 213 {
		t.Errorf("cases: %d, want 213", len(mx.Cases))
	}
}
