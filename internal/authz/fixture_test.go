package authz_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/authztest"
	"github.com/is-koala/koala-api/internal/authz/internal/matrix"
)

// Helpers shared by the Go matrix test and the OpenFGA conformance runner.

const matrixPath = "testdata/matrix.yaml"

// fixtureNamespace turns the matrix's symbolic user ids (u_worker) into
// UUIDs, because Subject ids are UUIDs. Both engines see the same UUIDs.
var fixtureNamespace = uuid.MustParse("6f1b0d7e-3c8a-4f5e-9a51-2b7c0e4d8a10")

func fixtureUserID(name string) uuid.UUID {
	return uuid.NewSHA1(fixtureNamespace, []byte(name))
}

// fixtureRef maps "user:<name>" to "user:<uuid>"; other references and
// wildcards are unchanged.
func fixtureRef(ref string) string {
	name, ok := strings.CutPrefix(ref, "user:")
	if !ok || name == authz.Wildcard {
		return ref
	}
	return "user:" + fixtureUserID(name).String()
}

// fixtureSubject returns the subject of a case; false for "anonymous".
func fixtureSubject(t *testing.T, s string) (authz.Subject, bool) {
	t.Helper()
	if s == "anonymous" {
		return authz.Subject{}, false
	}
	name, ok := strings.CutPrefix(s, "user:")
	if !ok {
		t.Fatalf("subject %q: want user:<id> or anonymous", s)
	}
	return authz.User(fixtureUserID(name)), true
}

func fixtureObject(t *testing.T, s string) authz.Object {
	t.Helper()
	o, ok := authz.ParseObject(fixtureRef(s))
	if !ok {
		t.Fatalf("object %q: want type:id", s)
	}
	return o
}

// fixtureTuples parses the matrix tuples with user ids mapped.
func fixtureTuples(t *testing.T, m *matrix.Matrix) []authz.Tuple {
	t.Helper()
	out := make([]authz.Tuple, 0, len(m.Tuples))
	for _, s := range m.Tuples {
		obj, rest, _ := strings.Cut(s, "#")
		rel, user, _ := strings.Cut(rest, "@")
		userRef, userRel, hasRel := strings.Cut(user, "#")
		mapped := fixtureRef(obj) + "#" + rel + "@" + fixtureRef(userRef)
		if hasRel {
			mapped += "#" + userRel
		}
		tp, err := authz.ParseTuple(mapped)
		if err != nil {
			t.Fatalf("fixture tuple %q: %v", s, err)
		}
		out = append(out, tp)
	}
	return out
}

func loadMatrix(t *testing.T) *matrix.Matrix {
	t.Helper()
	m, err := matrix.Load(matrixPath)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func loadModel(t *testing.T) *authz.Model {
	t.Helper()
	m, err := authz.DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// newMatrixEvaluator is the Go evaluator over the matrix fixtures.
func newMatrixEvaluator(t *testing.T, mx *matrix.Matrix) *authz.Evaluator {
	t.Helper()
	model := loadModel(t)
	tuples, err := authztest.NewTuples(model, fixtureTuples(t, mx)...)
	if err != nil {
		t.Fatal(err)
	}
	return authz.NewEvaluator(model, tuples)
}

// notRunReason says why a case does not run on both engines, or "" when
// it does.
func notRunReason(m *matrix.Matrix, c *matrix.Case) string {
	switch c.Engine {
	case matrix.EngineBoth:
		return ""
	case matrix.EngineSkip:
		return "skip: " + m.Actions[c.Action]
	}
	target := m.Actions[c.Action]
	switch {
	case c.Subject == "anonymous":
		return "go_only: anonymous subject, decided before authz (" + target + ")"
	case strings.HasPrefix(target, "Scope("):
		return "go_only: list filter " + target + ", Scoper arrives with the domain tables"
	case strings.HasPrefix(target, "go:"):
		return "go_only: rule outside the model " + target
	default:
		return "go_only: prefilter on " + target + " (" + c.Note + ")"
	}
}
