package softdelete_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	dbsrc "github.com/is-koala/koala-api/db"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// exampleCatalog is the catalog of the test databases' example tables,
// read from disk: unit tests do not import dbtest
// (scripts/test-integration.sh).
func exampleCatalog(t *testing.T) *softdelete.Catalog {
	t.Helper()
	examples, err := os.ReadFile("../dbtest/testdata/softdelete.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog(), examples)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestRealCatalogParses(t *testing.T) {
	t.Parallel()

	cat, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog())
	if err != nil {
		t.Fatal(err)
	}
	d, ok := cat.Table("deletions")
	if !ok || d.Class != softdelete.ClassLedger {
		t.Errorf("deletions = %+v, want class M1", d)
	}
	for _, tb := range cat.Tables() {
		if !tb.SoftDelete() && tb.Reason == "" {
			t.Errorf("%s: exempt without a reason", tb.Name)
		}
	}
}

func TestExampleCatalog(t *testing.T) {
	t.Parallel()

	cat := exampleCatalog(t)
	want := []string{"example_comments", "example_invoices", "example_notes", "example_owners", "example_projects", "example_tasks"}
	if got := cat.SoftDeleteTables(); !slices.Equal(got, want) {
		t.Errorf("soft-deleted tables = %v, want %v", got, want)
	}
	owners, _ := cat.Table("example_owners")
	if !owners.PII || owners.View() != "active_example_owners" {
		t.Errorf("example_owners = %+v, want pii and view active_example_owners", owners)
	}
	if n := len(cat.Edges()); n != 4 {
		t.Errorf("edges = %d, want 4", n)
	}
}

func TestParseCatalogRejects(t *testing.T) {
	t.Parallel()

	const base = "tables:\n  parents:\n    class: soft_delete\n  ledger:\n    class: M1\n    reason: evidence\n"
	tests := map[string]struct {
		docs []string
		want string
	}{
		"unknown key":             {[]string{"tables:\n  a:\n    class: soft_delete\n    kind: x\n"}, "unknown field"},
		"unknown top-level key":   {[]string{"table: {}\n"}, "unknown field"},
		"unknown class":           {[]string{"tables:\n  a:\n    class: M7\n"}, "class"},
		"exempt without reason":   {[]string{"tables:\n  a:\n    class: M2\n"}, "reason"},
		"pii on exempt":           {[]string{"tables:\n  a:\n    class: M2\n    reason: r\n    pii: true\n"}, "pii"},
		"bad table name":          {[]string{"tables:\n  A-b:\n    class: soft_delete\n"}, "name must match"},
		"view-like table name":    {[]string{"tables:\n  active_x:\n    class: soft_delete\n"}, "active_"},
		"table in two documents":  {[]string{base, "tables:\n  parents:\n    class: soft_delete\n"}, "twice"},
		"edge to unknown child":   {[]string{base + "edges:\n  - {child: nope, column: parent_id, parent: parents, policy: detach}\n"}, "child table"},
		"edge to exempt parent":   {[]string{base + "edges:\n  - {child: parents, column: ledger_id, parent: ledger, policy: detach}\n"}, "not soft-deleted"},
		"unknown policy":          {[]string{base + "edges:\n  - {child: ledger, column: parent_id, parent: parents, policy: orphan}\n"}, "policy"},
		"cascade to exempt child": {[]string{base + "edges:\n  - {child: ledger, column: parent_id, parent: parents, policy: cascade}\n"}, "cascade needs"},
		"edge listed twice": {[]string{base + "edges:\n" +
			"  - {child: ledger, column: parent_id, parent: parents, policy: restrict}\n" +
			"  - {child: ledger, column: parent_id, parent: parents, policy: detach}\n"}, "twice"},
		"unique exception without reason": {[]string{"tables:\n  a:\n    class: soft_delete\n    unique_exceptions:\n      a_key: ''\n"}, "unique exception"},
		"not YAML":                        {[]string{"tables: [\n"}, "parse YAML"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			docs := make([][]byte, len(tt.docs))
			for i, d := range tt.docs {
				docs[i] = []byte(d)
			}
			_, err := softdelete.ParseCatalog(docs...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// Delete checks its request before it touches the database, so a zero
// Querier is enough here.
func TestDeleteValidatesRequest(t *testing.T) {
	t.Parallel()

	d := softdelete.New(exampleCatalog(t))
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	someone := uuid.Must(uuid.NewV7())
	valid := softdelete.DeleteRequest{
		Table: "example_owners", ID: someone,
		Actor:  softdelete.Actor{Kind: softdelete.ActorUser, ID: someone},
		Reason: softdelete.ReasonUserRequest, At: at,
	}
	tests := map[string]struct {
		edit func(*softdelete.DeleteRequest)
		want string
	}{
		"unknown table":            {func(r *softdelete.DeleteRequest) { r.Table = "nope" }, "not a soft-deleted table"},
		"exempt table":             {func(r *softdelete.DeleteRequest) { r.Table = "deletions" }, "not a soft-deleted table"},
		"user without id":          {func(r *softdelete.DeleteRequest) { r.Actor.ID = uuid.Nil }, "needs an id"},
		"system with id":           {func(r *softdelete.DeleteRequest) { r.Actor.Kind = softdelete.ActorSystem }, "no id"},
		"unknown actor kind":       {func(r *softdelete.DeleteRequest) { r.Actor.Kind = "robot" }, "actor kind"},
		"unknown reason":           {func(r *softdelete.DeleteRequest) { r.Reason = "whim" }, "reason"},
		"missing application time": {func(r *softdelete.DeleteRequest) { r.At = time.Time{} }, "At is required"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := valid
			tt.edit(&r)
			_, err := d.Delete(t.Context(), db.Querier{}, r)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}
