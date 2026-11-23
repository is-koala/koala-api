package reference

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// TestChildren pins the reading of the parent LEFT JOIN child queries: no
// row is the parent's 404, a row without a child is an empty list (a
// faculty without departments exists in the data), and the rows keep the
// query's order.
func TestChildren(t *testing.T) {
	t.Parallel()

	_, err := children(nil)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != CodeReferenceItemNotFound || ae.Kind != apperr.KindNotFound {
		t.Fatalf("no row: %v, want %s", err, CodeReferenceItemNotFound)
	}

	items, err := children([]childRow{{}})
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("a parent without children: %v, %v; want an empty list", items, err)
	}

	a, b := uuid.New(), uuid.New()
	nameA, nameB := "ÇANKAYA", "ÇUBUK"
	items, err = children([]childRow{
		{ID: uuid.NullUUID{UUID: a, Valid: true}, Name: &nameA},
		{ID: uuid.NullUUID{UUID: b, Valid: true}, Name: &nameB},
	})
	if err != nil || !slices.Equal(items, []Item{{ID: a, Name: nameA}, {ID: b, Name: nameB}}) {
		t.Fatalf("children = %v, %v", items, err)
	}
}

// TestETagMatches pins the conditional request: the tag names the data
// version and its revision, and If-None-Match matches it under the weak
// comparison, in a list, or as "*"; another version or revision does not
// match.
func TestETagMatches(t *testing.T) {
	t.Parallel()

	tag := ETag(DataVersion{Version: 3, Revision: 2})
	if tag != `"reference-v3.2"` || ETag(DataVersion{Version: 3, Revision: 3}) == tag || ETag(DataVersion{Version: 4, Revision: 2}) == tag {
		t.Fatalf("ETag(3.2) = %s", tag)
	}
	for header, want := range map[string]bool{
		`"reference-v3.2"`:                     true,
		`W/"reference-v3.2"`:                   true,
		`"x", "reference-v3.2"`:                true,
		`"x" ,W/"reference-v3.2" `:             true,
		`*`:                                    true,
		`"reference-v3.1"`:                     false,
		`"reference-v2.2"`:                     false,
		`reference-v3.2`:                       false,
		`"reference-v3.2`:                      false,
		``:                                     false,
		`"reference-v3.20", "reference-v3.21"`: false,
	} {
		if got := Matches(header, tag); got != want {
			t.Errorf("Matches(%q, %s) = %v, want %v", header, tag, got, want)
		}
	}
}
