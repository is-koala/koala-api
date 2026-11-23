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
