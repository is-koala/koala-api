package softdeletetest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// Outcome is what the child's GET operations do once the parent on a
// detach edge is soft-deleted.
type Outcome string

// The outcomes a DetachCase may declare.
const (
	// Keeps is the rule of a detach edge: the child stays in the answer
	// and the fields that came from the parent read null.
	Keeps Outcome = "keeps"
	// Hides: the operation drops the child on purpose. A detail operation
	// then answers the 404 of a child that never existed (ADR-0014) and a
	// list leaves the child out.
	Hides Outcome = "hides"
	// Unauthenticated: the parent is the caller, so its deletion ends the
	// session and every operation answers 401 instead of hiding a row.
	Unauthenticated Outcome = "unauthenticated"
)

// Row is what a fixture created: the child row, the parent the test
// deletes, and what proves an answer carries the child.
type Row struct {
	// Child is the child row's id, uuid.Nil for a child without one (a
	// link row). An operation with a path parameter needs it.
	Child uuid.UUID
	// Parent is the row of the edge's parent table that the test deletes.
	Parent uuid.UUID
	// Shown proves an answer carries the child. It defaults to the
	// child's id; a child whose id is not in the answer names something
	// else only it puts there (a code, a name).
	Shown string
}

// shown returns the text that proves an answer carries the child.
func (r Row) shown() string {
	if r.Shown != "" {
		return r.Shown
	}
	return r.Child.String()
}

// DetachCase declares one detach edge of the catalog.
type DetachCase struct {
	// Unread says no GET operation reads the edge's child and why, in at
	// least softdelete.ExemptionReasonLen characters. JoinTest holds the
	// claim against the contract, so an operation added later fails the
	// test. The other fields stay empty.
	Unread string
	// Fixture creates one active child under one active parent. It opens
	// its own transactions; JoinTest does not wrap it in one, so it may
	// call services.
	Fixture func(ctx context.Context) (Row, error)
	// Authorize, when set, prepares each request about row: the
	// credentials of a caller that may read the child and the client
	// headers. It is called before and after the deletion, with the same
	// row, and the caller it sets up must survive the deletion unless the
	// outcome is Unauthenticated.
	Authorize func(ctx context.Context, row Row, r *http.Request) error
	// Outcome is what the child's operations do after the deletion.
	Outcome Outcome
	// Reason is why the child disappears; required unless the outcome is
	// Keeps, and at least softdelete.ExemptionReasonLen characters, the
	// threshold of its sibling rules. There is no silent exception to the
	// detach rule.
	Reason string
	// NullFields are the JSON fields of the child that came from the
	// parent. With Keeps they must read null (or be absent) after the
	// deletion: the row survives, the parent's data does not.
	NullFields []string
}

// JoinTest is the generated JOIN test over one contract and handler
// (ADR-0020 Karar 11'in kalan maddesi, ADR-0027 §2). ReadTest covers the
// parent's own endpoints; JoinTest covers the child's on a detach edge.
//
// On a detach edge the child stays when its parent is soft-deleted
// (ADR-0013 Karar 4), so the child's endpoints must keep answering with
// it: the row does not disappear and the fields that came from the parent
// read null. A query that reaches the parent by an inner join loses the
// row instead — the trap of Java #309/#321, which CheckJoins reads in the
// SQL and this test reads in the answer.
//
// Not every detach edge means the child to stay visible. A session whose
// device or user is gone is dead on purpose and the answer may not name a
// platform that no longer exists; an endpoint that reads the caller's own
// rows stops answering at all once the caller is deleted. JoinTest
// therefore makes every detach edge of the catalog declare what its
// child's endpoints do, with a reason when the child disappears: Keeps,
// Hides or Unauthenticated. An undeclared edge fails the test, so a detach
// edge added to db/softdelete.yaml turns it red before anything reads the
// new child.
type JoinTest struct {
	// Spec is the contract; its GET operations carry x-resource.
	Spec *openapi3.T
	// Handler serves Spec.
	Handler http.Handler
	// App is a koala_app pool on the database Handler reads; the test
	// deletes the parent through it.
	App *db.DB
	// Catalog classifies the tables and holds the edges.
	Catalog *softdelete.Catalog
	// Cases declares every detach edge of Catalog, keyed by
	// "<child>.<column>" as db/softdelete.yaml keys them.
	Cases map[string]DetachCase
	// Marked holds the detach parents some sqlc query exempts with the
	// inner-by-design marker (softdelete.MarkedParents over db.Queries()).
	// An edge whose parent is in it cannot be declared Keeps: the query
	// says the child goes. Empty means no query carries a marker, which is
	// right for a contract served by hand-written queries.
	Marked map[string]bool
}

// short reports whether a reason is below the threshold its sibling rules
// use (softdelete.ExemptionReasonLen, x-resource-reason).
func short(reason string) bool {
	return len([]rune(strings.TrimSpace(reason))) < softdelete.ExemptionReasonLen
}

// EdgeKey is the key of an edge in JoinTest.Cases.
func EdgeKey(e softdelete.Edge) string { return e.Child + "." + e.Column }

// edgeCase is one declared edge with the operations that read its child.
type edgeCase struct {
	edge softdelete.Edge
	decl DetachCase
	ops  []operation
}

// Run runs the test, one subtest per detach edge with operations.
func (jt JoinTest) Run(t *testing.T) {
	t.Helper()
	cases, problems := jt.plan()
	for _, p := range problems {
		t.Error(p)
	}
	if len(problems) > 0 {
		t.FailNow()
	}
	for i := range cases {
		c := cases[i]
		t.Run(EdgeKey(c.edge), func(t *testing.T) {
			t.Parallel()
			jt.runEdge(t, c)
		})
	}
}

// plan checks the contract against the catalog and the cases, without a
// database, and returns the edges to run and the problems found: a detach
// edge without a case, a case without an outcome or a fixture, a child that
// disappears without a reason, an Unread claim the contract contradicts,
// and a case on no edge of the catalog.
func (jt JoinTest) plan() ([]edgeCase, []string) {
	readers, problems := jt.readers()
	var out []edgeCase
	known := map[string]bool{}
	for _, e := range jt.Catalog.Edges() {
		if e.Policy != softdelete.Detach {
			continue
		}
		key := EdgeKey(e)
		known[key] = true
		decl, ok := jt.Cases[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("detach edge %s has no case: declare what %s's operations do once the parent is deleted, under the key %q", e, e.Child, key))
			continue
		}
		ops := readers[e.Child]
		if decl.Unread != "" {
			switch {
			case len(ops) > 0:
				problems = append(problems, fmt.Sprintf("%s: the case says no GET reads %s (%s), but GET %s does", e, e.Child, decl.Unread, ops[0].path))
			case decl.Fixture != nil || decl.Outcome != "":
				problems = append(problems, fmt.Sprintf("%s: an Unread case carries no fixture and no outcome", e))
			case short(decl.Unread):
				problems = append(problems, fmt.Sprintf("%s: say why nothing reads %s, in at least %d characters", e, e.Child, softdelete.ExemptionReasonLen))
			}
			continue
		}
		switch {
		case len(ops) == 0:
			problems = append(problems, fmt.Sprintf("%s: no GET operation reads %s; say so with Unread and a reason", e, e.Child))
		case decl.Fixture == nil:
			problems = append(problems, fmt.Sprintf("%s: the case needs a fixture that creates a child under an active parent", e))
		case decl.Outcome != Keeps && decl.Outcome != Hides && decl.Outcome != Unauthenticated:
			problems = append(problems, fmt.Sprintf("%s: outcome %q, want %s, %s or %s", e, decl.Outcome, Keeps, Hides, Unauthenticated))
		case decl.Outcome != Keeps && short(decl.Reason):
			problems = append(problems, fmt.Sprintf("%s: outcome %s needs a reason of at least %d characters; on a detach edge the child stays unless the query means otherwise",
				e, decl.Outcome, softdelete.ExemptionReasonLen))
		case decl.Outcome != Keeps && len(decl.NullFields) > 0:
			problems = append(problems, fmt.Sprintf("%s: NullFields belong to the %s outcome", e, Keeps))
		// The same decision is written twice: here and as the
		// inner-by-design marker of the child's query. The two are tied
		// so that removing one fails (ADR-0027 §2); dropping the marker
		// alone makes CheckJoins red.
		case decl.Outcome == Keeps && jt.Marked[e.Parent]:
			problems = append(problems, fmt.Sprintf("%s: a query exempts %s with the %#q marker, so the child does not stay; declare %s or %s, or drop the marker",
				e, e.Parent, softdelete.Exemption, Hides, Unauthenticated))
		default:
			out = append(out, edgeCase{edge: e, decl: decl, ops: ops})
		}
	}
	for _, key := range slices.Sorted(maps.Keys(jt.Cases)) {
		if !known[key] {
			problems = append(problems, fmt.Sprintf("the case %q is on no detach edge of the soft-delete catalog", key))
		}
	}
	return out, problems
}

// readers maps every table a GET operation names with x-resource to those
// operations. An x-resource that is not a string, or none, is ReadTest's
// business; readers skips it.
func (jt JoinTest) readers() (map[string][]operation, []string) {
	out := map[string][]operation{}
	var problems []string
	for _, path := range slices.Sorted(maps.Keys(jt.Spec.Paths.Map())) {
		op := jt.Spec.Paths.Value(path).Get
		if op == nil {
			continue
		}
		raw, ok := op.Extensions[Extension]
		if !ok {
			continue
		}
		table, ok := extensionString(raw)
		if !ok || table == "" || table == None {
			continue
		}
		params := len(pathParam.FindAllString(path, -1))
		if params > 1 {
			problems = append(problems, fmt.Sprintf("GET %s: operations with %d path parameters are not supported yet; extend softdeletetest", path, params))
			continue
		}
		out[table] = append(out[table], operation{
			path: path, params: params, table: table,
			self: extensionTrue(op.Extensions[ExtensionSelf]),
		})
	}
	return out, problems
}

func (jt JoinTest) runEdge(t *testing.T, c edgeCase) {
	t.Helper()
	ctx := t.Context()
	row, err := c.decl.Fixture(ctx)
	switch {
	case err != nil:
		t.Fatalf("fixture %s: %v", c.edge, err)
	case row.Parent == uuid.Nil:
		t.Fatalf("fixture %s returned no parent to delete", c.edge)
	}
	for _, op := range c.ops {
		if op.params > 0 && row.Child == uuid.Nil {
			t.Fatalf("GET %s has a path parameter, so the fixture of %s must return the child's id", op.path, c.edge)
		}
	}

	// Before the deletion every operation carries the child, so the
	// checks after it cannot pass by accident.
	for _, op := range c.ops {
		if err := jt.answer(ctx, c, op, row, true); err != nil {
			t.Error(err)
		}
	}

	err = jt.App.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		_, err := softdelete.New(jt.Catalog).Delete(ctx, q, softdelete.DeleteRequest{
			Table: c.edge.Parent, ID: row.Parent,
			Actor:  softdelete.Actor{Kind: softdelete.ActorSystem},
			Reason: softdelete.ReasonRetention, At: time.Now(),
		})
		if err != nil {
			return fmt.Errorf("soft-delete: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("delete %s %s: %v", c.edge.Parent, row.Parent, err)
	}
	for _, op := range c.ops {
		if err := jt.answer(ctx, c, op, row, false); err != nil {
			t.Error(err)
		}
	}
}

// answer checks what op says about the child: before the deletion it
// always carries it; after it, what the case declares.
func (jt JoinTest) answer(ctx context.Context, c edgeCase, op operation, row Row, active bool) error {
	path := op.path
	if op.params > 0 {
		path = pathParam.ReplaceAllString(op.path, row.Child.String())
	}
	status, body := jt.get(ctx, c, row, path)
	carries := strings.Contains(body, row.shown())

	if active {
		if status != http.StatusOK || !carries {
			return fmt.Errorf("GET %s under an active %s = %d %s, want 200 carrying %s",
				op.path, c.edge.Parent, status, body, row.shown())
		}
		return nil
	}

	switch c.decl.Outcome {
	case Unauthenticated:
		if status != http.StatusUnauthorized || carries {
			return fmt.Errorf("GET %s after %s %s was deleted = %d %s, want 401 without %s (%s)",
				op.path, c.edge.Parent, row.Parent, status, body, row.shown(), c.decl.Reason)
		}
		return nil
	case Hides:
		if op.params == 0 {
			if status != http.StatusOK || carries {
				return fmt.Errorf("GET %s after %s was deleted = %d %s, want 200 without %s (%s)",
					op.path, c.edge.Parent, status, body, row.shown(), c.decl.Reason)
			}
			return nil
		}
		missing := pathParam.ReplaceAllString(op.path, uuid.NewString())
		missingStatus, missingBody := jt.get(ctx, c, row, missing)
		if status != http.StatusNotFound || status != missingStatus || body != missingBody {
			return fmt.Errorf("GET %s for a child whose %s is deleted = %d %s; for a child that never existed = %d %s; want the same 404 (%s)",
				op.path, c.edge.Parent, status, body, missingStatus, missingBody, c.decl.Reason)
		}
		return nil
	case Keeps:
		if status != http.StatusOK || !carries {
			return fmt.Errorf("GET %s after %s %s was deleted = %d %s, want 200 still carrying %s: on a detach edge the child stays",
				op.path, c.edge.Parent, row.Parent, status, body, row.shown())
		}
		if err := nullFields(body, row, c.decl.NullFields); err != nil {
			return fmt.Errorf("GET %s after %s was deleted: %w", op.path, c.edge.Parent, err)
		}
		return nil
	default:
		// plan() refuses any other outcome before a request is sent.
		return fmt.Errorf("%s: unknown outcome %q", c.edge, c.decl.Outcome)
	}
}

// get requests path as the caller Authorize sets up for row.
func (jt JoinTest) get(ctx context.Context, c edgeCase, row Row, path string) (int, string) {
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
	req.Header.Set(httpx.HeaderRequestID, requestID)
	if c.decl.Authorize != nil {
		if err := c.decl.Authorize(ctx, row, req); err != nil {
			return 0, "authorize: " + err.Error()
		}
	}
	rec := httptest.NewRecorder()
	jt.Handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// nullFields checks that the child's fields that came from the parent read
// null, or are absent, once the parent is deleted. A field that vanishes
// from the body counts as null; the first real Keeps endpoint should
// narrow that to "present and null" (ADR-0027 §2, izleyen kalem).
func nullFields(body string, row Row, fields []string) error {
	if len(fields) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		return fmt.Errorf("decode the body %s: %w", body, err)
	}
	obj := findByID(decoded, row.Child.String())
	if obj == nil {
		return fmt.Errorf("the body %s has no object whose id is %s", body, row.Child)
	}
	for _, f := range fields {
		if got, ok := obj[f]; ok && got != nil {
			return fmt.Errorf("the child's %s = %v, want null once its parent is deleted", f, got)
		}
	}
	return nil
}

// findByID returns the JSON object whose "id" is id, at any depth.
func findByID(v any, id string) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		if got, ok := x["id"].(string); ok && got == id {
			return x
		}
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if got := findByID(x[k], id); got != nil {
				return got
			}
		}
	case []any:
		for _, item := range x {
			if got := findByID(item, id); got != nil {
				return got
			}
		}
	}
	return nil
}
