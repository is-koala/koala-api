// Package reference serves the reference lists of /v1/reference: countries,
// a country's cities, a city's districts; universities, a university's
// faculties, a faculty's departments (ADR-0029 Karar 8, ADR-0037).
//
// The data is M4: read-only for the application. Its rows come from the
// versioned files of db/referencedata, which `koala migrate up` brings the
// tables in line with; a dropped row is retired, never deleted. A list
// shows the rows in use, ordered by name under the Turkish collation, and
// is one page (the contract's ReferenceItemList). A child list of a parent
// that does not exist, or is retired, is REFERENCE_ITEM_NOT_FOUND; a
// parent in use without children is an empty list. The operations are
// public (MED-012).
package reference

import (
	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// CodeReferenceItemNotFound: the parent of a list does not exist or is
// retired. The education records answer the same code for a reference
// they cannot name (internal/users).
const CodeReferenceItemNotFound apperr.Code = "REFERENCE_ITEM_NOT_FOUND"

// Codes lists every code this package defines, for the contract test.
func Codes() []apperr.Code { return []apperr.Code{CodeReferenceItemNotFound} }

// Item is one row of a reference list.
type Item struct {
	ID   uuid.UUID
	Name string
}
