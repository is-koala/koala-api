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
	if g, _ := cat.Table("goose_db_version"); g.Class != softdelete.ClassTool {
		t.Errorf("goose_db_version = %+v, want class tool", g)
	}
	for _, tb := range cat.Tables() {
		if !tb.SoftDelete() && tb.Reason == "" {
			t.Errorf("%s: exempt without a reason", tb.Name)
		}
	}
}

func TestExampleCatalog(t *testing.T) {
	t.Parallel()

	// The examples are merged with the real catalog; only their part is
	// pinned here, so real tables can join without touching this test.
	cat := exampleCatalog(t)
	want := []string{"example_comments", "example_folders", "example_invoices", "example_notes", "example_owners", "example_projects", "example_tasks"}
	var got []string
	for _, name := range cat.SoftDeleteTables() {
		if strings.HasPrefix(name, "example_") {
			got = append(got, name)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("soft-deleted example tables = %v, want %v", got, want)
	}
	owners, _ := cat.Table("example_owners")
	if !owners.PII || owners.View() != "active_example_owners" {
		t.Errorf("example_owners = %+v, want pii and view active_example_owners", owners)
	}
	edges := 0
	for _, e := range cat.Edges() {
		if strings.HasPrefix(e.Child, "example_") {
			edges++
		}
	}
	if edges != 7 {
		t.Errorf("example edges = %d, want 7", edges)
	}
}

func TestParseCatalogRejects(t *testing.T) {
	t.Parallel()

	const base = "tables:\n  parents:\n    class: soft_delete\n  ledger:\n    class: M1\n    reason: evidence\n"
	tests := map[string]struct {
		docs []string
		want string
	}{
		"unknown key":             {[]string{"tables:\n  a:\n    class: soft_delete\n    kind: x\n"}, "field kind not found"},
		"unknown top-level key":   {[]string{"table: {}\n"}, "field table not found"},
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
		"tool without reason":             {[]string{"tables:\n  a:\n    class: tool\n"}, "reason"},
		"name over the limit": {[]string{"tables:\n  " + strings.Repeat("n", softdelete.MaxTableName+1) + ":\n    class: soft_delete\n"},
			"at most 46 bytes"},
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

// TestCatalogYAMLIsStrict pins the YAML reader (go.yaml.in/yaml/v3 with
// KnownFields, ADR-0020): every way a hand-edited catalog can silently mean
// something else is an error.
func TestCatalogYAMLIsStrict(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ doc, want string }{
		"duplicate table":          {"tables:\n  a:\n    class: soft_delete\n  a:\n    class: M1\n    reason: r\n", "already defined"},
		"duplicate key in a table": {"tables:\n  a:\n    class: M1\n    class: soft_delete\n", "already defined"},
		"second document":          {"tables:\n  a:\n    class: soft_delete\n---\ntables: {}\n", "more than one document"},
		"empty":                    {"", "empty document"},
		"only a comment":           {"# nothing\n", "empty document"},
		"wrong type":               {"tables:\n  - a\n", "cannot unmarshal"},
		"pii as a string":          {"tables:\n  a:\n    class: soft_delete\n    pii: maybe\n", "cannot unmarshal"},
		"unknown edge key":         {"tables:\n  a:\n    class: soft_delete\nedges:\n  - {child: a, column: b_id, parent: a, policy: detach, why: x}\n", "field why not found"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := softdelete.ParseCatalog([]byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}

	// The limit is shared with softdeletegen: exactly MaxTableName bytes is
	// accepted.
	ok := "tables:\n  " + strings.Repeat("n", softdelete.MaxTableName) + ":\n    class: soft_delete\n"
	if _, err := softdelete.ParseCatalog([]byte(ok)); err != nil {
		t.Errorf("name of %d bytes: %v", softdelete.MaxTableName, err)
	}
	if softdelete.MaxTableName != 46 {
		t.Errorf("MaxTableName = %d, want 46 (63 - len(\"_deleted_together\"))", softdelete.MaxTableName)
	}
}
