package softdeletetest

// The frozen-operation skip of ADR-0029 §1 lives in two files, readers()
// here and plan() in readtest.go. readtest_internal_test.go binds the
// second; TestReadersSkipFrozenOperations below binds this one, so the
// same rule is not tested in one place only.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// The negative cases of JoinTest, without a database: a catalog, a
// contract or a handler that breaks the detach rule makes it fail.

// detachEdge is the one edge of detachCatalog.
var detachEdge = softdelete.Edge{Child: "comments", Column: "author_id", Parent: "owners", Policy: softdelete.Detach}

// detachCatalog is a two-table catalog with one detach edge, so a valid
// case list is one entry long.
func detachCatalog(t *testing.T) *softdelete.Catalog {
	t.Helper()
	cat, err := softdelete.ParseCatalog([]byte(`tables:
  owners:
    class: soft_delete
  comments:
    class: soft_delete
edges:
  - child: comments
    column: author_id
    parent: owners
    policy: detach
`))
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func noRow(context.Context) (Row, error) { return Row{}, nil }

func TestJoinPlanReportsProblems(t *testing.T) {
	t.Parallel()

	const key = "comments.author_id"
	reader := map[string]any{"/comments": "comments"}
	tests := map[string]struct {
		resources map[string]any
		cases     map[string]DetachCase
		want      string
	}{
		"edge without a case": {reader, nil, "has no case"},
		"unread but read": {reader, map[string]DetachCase{
			key: {Unread: "nothing reads it, and nothing is going to for a while"},
		}, "but GET /comments does"},
		"unread with a fixture": {map[string]any{"/livez": nil}, map[string]DetachCase{
			key: {Unread: "nothing reads it, and nothing is going to for a while", Fixture: noRow},
		}, "carries no fixture and no outcome"},
		"read by nothing and not declared so": {map[string]any{"/livez": nil}, map[string]DetachCase{
			key: {Fixture: noRow, Outcome: Keeps},
		}, "say so with Unread"},
		"missing fixture": {reader, map[string]DetachCase{
			key: {Outcome: Keeps},
		}, "needs a fixture"},
		"unknown outcome": {reader, map[string]DetachCase{
			key: {Fixture: noRow, Outcome: "maybe"},
		}, `outcome "maybe"`},
		"a child that disappears without a reason": {reader, map[string]DetachCase{
			key: {Fixture: noRow, Outcome: Hides},
		}, "needs a reason"},
		"a reason too short to say anything": {reader, map[string]DetachCase{
			key: {Fixture: noRow, Outcome: Hides, Reason: "it is dead"},
		}, "needs a reason of at least"},
		"an Unread reason too short": {map[string]any{"/livez": nil}, map[string]DetachCase{
			key: {Unread: "nothing reads it"},
		}, "in at least"},
		"null fields on a child that disappears": {reader, map[string]DetachCase{
			key: {Fixture: noRow, Outcome: Hides, Reason: "the session is dead without its device, so it has no platform", NullFields: []string{"platform"}},
		}, "NullFields belong"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, problems := JoinTest{
				Spec: spec(tt.resources), Catalog: detachCatalog(t), Cases: tt.cases,
			}.plan()
			if len(problems) != 1 || !strings.Contains(problems[0], tt.want) {
				t.Fatalf("problems = %q, want one containing %q", problems, tt.want)
			}
		})
	}

	// An operation with two path parameters is not supported yet; the
	// edge then looks unread as well.
	t.Run("two path parameters", func(t *testing.T) {
		t.Parallel()
		_, problems := JoinTest{
			Spec:    spec(map[string]any{"/users/{userId}/comments/{commentId}": "comments"}),
			Catalog: detachCatalog(t),
			Cases:   map[string]DetachCase{key: {Fixture: noRow, Outcome: Keeps}},
		}.plan()
		if len(problems) != 2 || !strings.Contains(problems[0], "2 path parameters") {
			t.Fatalf("problems = %q, want the unsupported operation first", problems)
		}
	})

	// The marker in the SQL and the declaration here say the same thing;
	// Keeps contradicts the marker (ADR-0027 §2).
	t.Run("Keeps against an inner-by-design marker", func(t *testing.T) {
		t.Parallel()
		_, problems := JoinTest{
			Spec:    spec(reader),
			Catalog: detachCatalog(t),
			Cases:   map[string]DetachCase{key: {Fixture: noRow, Outcome: Keeps}},
			Marked:  map[string]bool{"comments.author_id": true},
		}.plan()
		if len(problems) != 1 || !strings.Contains(problems[0], "does not stay") {
			t.Fatalf("problems = %q, want one about the marker contradicting Keeps", problems)
		}
	})
	t.Run("Hides beside an inner-by-design marker", func(t *testing.T) {
		t.Parallel()
		cases, problems := JoinTest{
			Spec:    spec(reader),
			Catalog: detachCatalog(t),
			Cases: map[string]DetachCase{key: {
				Fixture: noRow, Outcome: Hides,
				Reason: "the query destroys the nullable side on purpose and fails closed",
			}},
			Marked: map[string]bool{"comments.author_id": true},
		}.plan()
		if len(problems) > 0 || len(cases) != 1 {
			t.Fatalf("cases = %v, problems = %q; want the edge and no problem", cases, problems)
		}
	})

	t.Run("a case on no edge", func(t *testing.T) {
		t.Parallel()
		_, problems := JoinTest{
			Spec: spec(reader), Catalog: detachCatalog(t),
			Cases: map[string]DetachCase{
				key:            {Fixture: noRow, Outcome: Keeps},
				"comments.old": {Unread: "an edge that was removed from the catalog long ago"},
			},
		}.plan()
		if len(problems) != 1 || !strings.Contains(problems[0], "is on no detach edge") {
			t.Fatalf("problems = %q, want one about a stale case", problems)
		}
	})

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		cases, problems := JoinTest{
			Spec: spec(map[string]any{
				"/comments":      "comments",
				"/comments/{id}": "comments",
				"/owners/{id}":   "owners", // the parent's own operation is ReadTest's
				"/livez":         nil,
			}),
			Catalog: detachCatalog(t),
			Cases: map[string]DetachCase{
				key: {Fixture: noRow, Outcome: Keeps, NullFields: []string{"author"}},
			},
		}.plan()
		if len(problems) > 0 || len(cases) != 1 || len(cases[0].ops) != 2 {
			t.Fatalf("cases = %v, problems = %q; want the edge with its two operations", cases, problems)
		}
	})
}

// A detach edge whose child the contract does not read yet is declared
// Unread, and the claim alone is enough.
func TestJoinPlanAcceptsAnUnreadEdge(t *testing.T) {
	t.Parallel()

	cases, problems := JoinTest{
		Spec:    spec(map[string]any{"/owners/{id}": "owners"}),
		Catalog: detachCatalog(t),
		Cases: map[string]DetachCase{
			"comments.author_id": {Unread: "no GET operation reads comments yet; the first one will declare an outcome"},
		},
	}.plan()
	if len(problems) > 0 || len(cases) != 0 {
		t.Fatalf("cases = %v, problems = %q; want neither", cases, problems)
	}
}

// TestReadersSkipFrozenOperations: a frozen operation (ADR-0029 §1) is no
// reader, so an edge whose only reader is frozen is still Unread and the
// claim holds. Its companion is "unread but read" in
// TestJoinPlanReportsProblems: the same operation without the marker makes
// the Unread claim a problem, which is what arms this test again when the
// handler lands.
func TestReadersSkipFrozenOperations(t *testing.T) {
	t.Parallel()

	s := spec(map[string]any{"/comments": "comments"})
	s.Paths.Value("/comments").Get.Extensions[ExtensionNotImplemented] = "handler lands in a later PR"
	cases, problems := JoinTest{
		Spec:    s,
		Catalog: detachCatalog(t),
		Cases: map[string]DetachCase{
			"comments.author_id": {Unread: "the only GET that reads comments is still frozen"},
		},
	}.plan()
	if len(problems) > 0 || len(cases) != 0 {
		t.Fatalf("cases = %v, problems = %q; want neither", cases, problems)
	}
}

func TestAnswerCatchesLeaks(t *testing.T) {
	t.Parallel()

	child := uuid.Must(uuid.NewV7())
	row := Row{Child: child, Parent: uuid.Must(uuid.NewV7())}
	detail := operation{path: "/comments/{id}", params: 1, table: "comments"}
	list := operation{path: "/comments", table: "comments"}
	ok := func(string) int { return http.StatusOK }
	notFound := func(string) int { return http.StatusNotFound }
	unauthorized := func(string) int { return http.StatusUnauthorized }
	same := func(string) string { return `{"code":"COMMENT_NOT_FOUND"}` }
	withAuthor := func(string) string {
		return `{"items":[{"id":"` + child.String() + `","author":{"id":"x"}}]}`
	}
	withoutAuthor := func(string) string {
		return `{"items":[{"id":"` + child.String() + `","author":null}]}`
	}
	empty := func(string) string { return `{"items":[]}` }

	keeps := DetachCase{Outcome: Keeps, NullFields: []string{"author"}}
	hides := DetachCase{Outcome: Hides, Reason: "a session whose device is gone is dead"}
	gone := DetachCase{Outcome: Unauthenticated, Reason: "the parent is the caller"}

	tests := map[string]struct {
		h      http.Handler
		decl   DetachCase
		op     operation
		active bool
		want   string // "" when the check passes
	}{
		"the child disappeared although the edge detaches": {handler(ok, empty), keeps, list, false, "the child stays"},
		"the child's endpoint broke":                       {handler(notFound, same), keeps, detail, false, "the child stays"},
		"the parent's data survived the deletion":          {handler(ok, withAuthor), keeps, list, false, "want null"},
		"the child stayed without its parent's data":       {handler(ok, withoutAuthor), keeps, list, false, ""},
		"an active child is not served":                    {handler(notFound, same), keeps, detail, true, "want 200 carrying"},
		"an active child is served":                        {handler(ok, withoutAuthor), keeps, list, true, ""},
		"a hidden child is still listed":                   {handler(ok, withAuthor), hides, list, false, "want 200 without"},
		"a hidden child is left out":                       {handler(ok, empty), hides, list, false, ""},
		"a hidden child still has a detail":                {handler(ok, withAuthor), hides, detail, false, "want the same 404"},
		"a hidden child answers like a missing one":        {handler(notFound, same), hides, detail, false, ""},
		"the deleted caller is still served":               {handler(ok, withAuthor), gone, list, false, "want 401"},
		"the deleted caller is refused":                    {handler(unauthorized, same), gone, list, false, ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := edgeCase{edge: detachEdge, decl: tt.decl}
			err := JoinTest{Handler: tt.h}.answer(t.Context(), c, tt.op, row, tt.active)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("err = %v, want none", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}
