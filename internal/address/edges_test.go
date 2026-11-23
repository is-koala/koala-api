package address_test

import (
	"testing"

	dbsrc "github.com/is-koala/koala-api/db"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// addressEdgeFindings are the catalog's foreign keys to addresses whose
// policy is not restrict.
func addressEdgeFindings(cat *softdelete.Catalog) []softdelete.Edge {
	var out []softdelete.Edge
	for _, e := range cat.Edges() {
		if e.Parent == "addresses" && e.Policy != softdelete.Restrict {
			out = append(out, e)
		}
	}
	return out
}

// TestAddressConsumersAreRestrictEdges holds ADR-0039 Karar 5 for every
// consumer of an address, the profile's primary link today and a company's
// or a job's tomorrow: it points at the address on a restrict edge. A
// detach edge would let the owner delete an address a profile, a company
// or a job still shows; a cascade edge would delete them with it. The
// demo catalogs prove the rule runs: one consumer edge of each policy,
// and only restrict passes.
func TestAddressConsumersAreRestrictEdges(t *testing.T) {
	t.Parallel()

	catalog, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range addressEdgeFindings(catalog) {
		t.Errorf("%s points at addresses with policy %s; a consumer of an address is a restrict edge (ADR-0039 Karar 5)", e, e.Policy)
	}
	consumers := 0
	for _, e := range catalog.Edges() {
		if e.Parent == "addresses" {
			consumers++
		}
	}
	if consumers == 0 {
		t.Fatal("no edge points at addresses; the profile's primary address link is missing from the catalog")
	}

	for policy, red := range map[string]bool{"restrict": false, "detach": true, "cascade": true} {
		demo, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog(), []byte(
			"edges:\n  - child: users\n    column: work_address_id\n    parent: addresses\n    policy: "+policy+"\n"))
		if err != nil {
			t.Fatalf("demo catalog with a %s edge: %v", policy, err)
		}
		if got := len(addressEdgeFindings(demo)) > 0; got != red {
			t.Errorf("a %s consumer edge: finding %v, want %v", policy, got, red)
		}
	}
}
