package softdeletetest

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	dbsrc "github.com/is-koala/koala-api/db"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// The negative cases of ReadTest, without a database: a contract or a
// handler that breaks the soft-delete read rules makes it fail.

func catalog(t *testing.T) *softdelete.Catalog {
	t.Helper()
	examples, err := os.ReadFile("../../dbtest/testdata/softdelete.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog(), examples)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

// spec builds a contract of GET operations; a nil resource leaves the
// operation without x-resource.
func spec(resources map[string]any) *openapi3.T {
	paths := openapi3.NewPaths()
	for path, resource := range resources {
		op := openapi3.NewOperation()
		if resource != nil {
			op.Extensions = map[string]any{Extension: resource}
		}
		paths.Set(path, &openapi3.PathItem{Get: op})
	}
	return &openapi3.T{Paths: paths}
}

func noFixture(context.Context, db.Querier) (uuid.UUID, error) { return uuid.Nil, nil }

func TestPlanReportsContractProblems(t *testing.T) {
	t.Parallel()

	owners := map[string]Fixture{"example_owners": noFixture}
	tests := map[string]struct {
		resources map[string]any
		fixtures  map[string]Fixture
		want      string
	}{
		"no x-resource":       {map[string]any{"/livez": nil}, owners, "no GET operation carries"},
		"not a table name":    {map[string]any{"/x": 42}, owners, "must be a table name"},
		"unknown table":       {map[string]any{"/x": "nope"}, owners, "not in the soft-delete catalog"},
		"missing fixture":     {map[string]any{"/example-projects/{id}": "example_projects"}, owners, "no fixture for the soft-deleted table example_projects"},
		"two path parameters": {map[string]any{"/a/{a}/owners/{id}": "example_owners"}, owners, "2 path parameters"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, problems := ReadTest{Spec: spec(tt.resources), Catalog: catalog(t), Fixtures: tt.fixtures}.plan()
			if len(problems) != 1 || !strings.Contains(problems[0], tt.want) {
				t.Fatalf("problems = %q, want one containing %q", problems, tt.want)
			}
		})
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		ops, problems := ReadTest{
			Spec: spec(map[string]any{
				"/example-owners":      "example_owners",
				"/example-owners/{id}": "example_owners",
				"/receipts/{id}":       "example_receipts", // exempt: nothing to hide
				"/catalog":             None,               // reads no table
				"/livez":               nil,
			}),
			Catalog:  catalog(t),
			Fixtures: owners,
		}.plan()
		if len(problems) > 0 || len(ops) != 1 || len(ops["example_owners"]) != 2 {
			t.Fatalf("ops = %v, problems = %q; want two operations on example_owners", ops, problems)
		}
	})
}

// handler answers every request with status and a body built from the
// path.
func handler(status func(path string) int, body func(path string) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status(r.URL.Path))
		_, _ = w.Write([]byte(body(r.URL.Path)))
	})
}

func TestVisibilityCatchesLeaks(t *testing.T) {
	t.Parallel()

	id := uuid.Must(uuid.NewV7())
	deleted := "/owners/" + id.String()
	detail := operation{path: "/owners/{id}", params: 1}
	list := operation{path: "/owners", params: 0}
	self := operation{path: "/me", self: true}
	unauthenticated := func(string) int { return http.StatusUnauthorized }
	notFound := func(string) int { return http.StatusNotFound }
	ok := func(string) int { return http.StatusOK }
	same := func(string) string { return `{"code":"OWNER_NOT_FOUND"}` }

	tests := map[string]struct {
		h       http.Handler
		op      operation
		visible bool
		want    string // "" when the check passes
	}{
		"deleted object still served": {handler(ok, same), detail, false, "want the same 404"},
		"404 body names the object":   {handler(notFound, func(p string) string { return `{"path":"` + p + `"}` }), detail, false, "want the same 404"},
		"deleted 404 differs from missing": {handler(func(p string) int {
			if p == deleted {
				return http.StatusGone
			}
			return http.StatusNotFound
		}, same), detail, false, "want the same 404"},
		"hidden like a missing object": {handler(notFound, same), detail, false, ""},
		"active object not served":     {handler(notFound, same), detail, true, "want 200"},
		"deleted object still listed":  {handler(ok, func(string) string { return `{"items":[{"id":"` + id.String() + `"}]}` }), list, false, "lists"},
		"active object not listed":     {handler(ok, func(string) string { return `{"items":[]}` }), list, true, "lists"},
		"list fails":                   {handler(notFound, same), list, false, "want 200"},
		"deleted object left out":      {handler(ok, func(string) string { return `{"items":[]}` }), list, false, ""},
		"deleted caller still served":  {handler(ok, func(string) string { return `{"id":"` + id.String() + `"}` }), self, false, "want 401"},
		"deleted caller refused":       {handler(unauthenticated, same), self, false, ""},
		"active caller refused":        {handler(unauthenticated, same), self, true, "want 200"},
		"active caller served":         {handler(ok, func(string) string { return `{"id":"` + id.String() + `"}` }), self, true, ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := ReadTest{Handler: tt.h}.visibility(t.Context(), tt.op, id, tt.visible)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("err = %v, want none", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}
