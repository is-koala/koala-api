package authz_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/internal/matrix"
)

// concealExceptions are deny rows whose caller does not reject, so
// Decision.Conceal does not apply.
var concealExceptions = map[string]string{
	"AZM-055": "field-level projection: the public view of the job",
	"AZM-131": "WebSocket subscription refusal; there is no 404 on a socket",
	"AZM-151": "field-level projection: plate and driver hidden",
	"AZM-163": "field-level projection: no CV link",
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
				if !basisHolds(mx, c.Basis, d.Basis) {
					t.Errorf("basis %q does not show the matrix basis %q (%s)", d.Basis, c.Basis, mx.Basis[c.Basis])
				}
				return
			}
			if len(d.Basis) != 0 {
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
	for id := range concealExceptions {
		if !hasDualCase(mx, id) {
			t.Errorf("exception %s is not a [go, openfga] row of the matrix", id)
		}
	}
}

// basisHolds reports whether the evaluator's basis shows the matrix's
// basis key: a step on the relation the key's description names, or a
// wildcard grant for "public".
func basisHolds(mx *matrix.Matrix, key string, basis authz.Basis) bool {
	if len(basis) == 0 {
		return false
	}
	if rel, ok := mx.BasisRelation(key); ok {
		return slices.Contains(basis.Relations(), rel)
	}
	if key == "public" {
		for _, p := range basis {
			if p.Grant.IsWildcard() {
				return true
			}
		}
	}
	return false
}

// TestMatrixStaffBasisNamesTheRole: every allowed staff row's basis
// passes through a staff role object, staff_role:<id>, so the audit trail
// shows which role granted the permission (ADR-0026).
func TestMatrixStaffBasisNamesTheRole(t *testing.T) {
	t.Parallel()
	mx := loadMatrix(t)
	ev := newMatrixEvaluator(t, mx)
	seen := 0
	for i := range mx.Cases {
		c := &mx.Cases[i]
		if !strings.HasPrefix(c.Subject, "staff:") || !c.Allowed() {
			continue
		}
		seen++
		s, _ := fixtureSubject(t, c.Subject)
		_, relation, _ := mx.Relation(c.Action)
		d, err := ev.Check(context.Background(), s, authz.Action(relation), fixtureObject(t, c.Object))
		if err != nil || !d.Allowed {
			t.Fatalf("%s: %+v, %v", c.ID, d, err)
		}
		if !strings.Contains(d.Basis.String(), "staff_role:") {
			t.Errorf("%s: basis %q names no staff_role", c.ID, d.Basis)
		}
	}
	if seen == 0 {
		t.Fatal("no allowed staff row")
	}
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
	counts := map[matrix.Engine]int{}
	for i := range mx.Cases {
		counts[mx.Cases[i].Engine]++
	}
	// 98 staff rows (pairs S01-S26, ADR-0026) run on both engines. The
	// Faz 2 pairs of ADR-0029 added 7 dual rows (company.follow and
	// company_invitation.revoke) and 13 go_only ones (the self-scoped
	// surface and the identity attribute).
	want := map[matrix.Engine]int{matrix.EngineBoth: 258, matrix.EngineGoOnly: 68, matrix.EngineSkip: 6}
	for engine, n := range want {
		if counts[engine] != n {
			t.Errorf("%s rows: %d, want %d", engine, counts[engine], n)
		}
	}
	if len(mx.Cases) != 332 {
		t.Errorf("cases: %d, want 332", len(mx.Cases))
	}
}
