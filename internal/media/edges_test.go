package media_test

import (
	"testing"

	dbsrc "github.com/is-koala/koala-api/db"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// mediaEdgeFindings are the catalog's foreign keys to media whose policy
// is not restrict.
func mediaEdgeFindings(cat *softdelete.Catalog) []softdelete.Edge {
	var out []softdelete.Edge
	for _, e := range cat.Edges() {
		if e.Parent == "media" && e.Policy != softdelete.Restrict {
			out = append(out, e)
		}
	}
	return out
}

// TestMediaConsumersAreRestrictEdges holds ADR-0035 Karar 7: a table that
// points at a media does so on a restrict edge. A detach edge would let
// the owner delete a media that a profile or a gallery still shows; a
// cascade edge would delete the profile or the gallery with it.
//
// Today no table points at media, so the check over the real catalog
// passes on an empty set; it means something from the first consumer on.
// The demo catalogs below prove the rule runs: one consumer edge of each
// policy, and only restrict passes.
func TestMediaConsumersAreRestrictEdges(t *testing.T) {
	t.Parallel()

	catalog, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range mediaEdgeFindings(catalog) {
		t.Errorf("%s points at media with policy %s; a consumer of media is a restrict edge (ADR-0035 Karar 7)", e, e.Policy)
	}

	for policy, red := range map[string]bool{"restrict": false, "detach": true, "cascade": true} {
		demo, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog(), []byte(
			"edges:\n  - child: users\n    column: avatar_media_id\n    parent: media\n    policy: "+policy+"\n"))
		if err != nil {
			t.Fatalf("demo catalog with a %s edge: %v", policy, err)
		}
		if got := len(mediaEdgeFindings(demo)) > 0; got != red {
			t.Errorf("a %s consumer edge: finding %v, want %v", policy, got, red)
		}
	}
}
