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
//
// A list is cacheable for a day and carries an ETag derived from the data
// version the tables hold and its revision (reference_data_version); a
// matching If-None-Match is 304 without a body.
package reference

import (
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// CodeReferenceItemNotFound is the answer to a list whose parent does not
// exist or is retired. The education records answer the same code for a
// reference they cannot name (internal/users).
const CodeReferenceItemNotFound apperr.Code = "REFERENCE_ITEM_NOT_FOUND"

// Codes lists every code this package defines, for the contract test.
func Codes() []apperr.Code { return []apperr.Code{CodeReferenceItemNotFound} }

// Item is one row of a reference list.
type Item struct {
	ID   uuid.UUID
	Name string
}

// CacheControl is every list's Cache-Control: public data that changes
// once a year at most, cacheable by clients and shared caches for a day.
const CacheControl = "public, max-age=86400"

// DataVersion is the data version the tables hold and its revision: a
// sync that repairs the rows under the same version moves the revision.
type DataVersion struct {
	Version  int32
	Revision int32
}

// ETag is the entity tag of every list at a data version and revision.
// Every list of one revision is one representation per URL, so the pair
// names it: a repair changes the tag as a new version does, and a client
// that holds the repaired list gets the new one. A change of the
// representation itself (a new field of ReferenceItem) must change this
// prefix.
func ETag(v DataVersion) string {
	return `"reference-v` + strconv.FormatInt(int64(v.Version), 10) + "." + strconv.FormatInt(int64(v.Revision), 10) + `"`
}

// Matches reports whether an If-None-Match header value matches etag:
// "*", or one of its comma-separated entity tags under the weak
// comparison of RFC 9110 §8.8.3.2 (a W/ prefix is ignored).
func Matches(ifNoneMatch, etag string) bool {
	for _, tag := range strings.Split(ifNoneMatch, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.TrimPrefix(tag, "W/") == strings.TrimPrefix(etag, "W/") {
			return true
		}
	}
	return false
}
