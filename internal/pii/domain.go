// Package pii is the PII gate (plan §5.3, §5.5; ADR-0026). Personal data
// of a person (TCKN, full IBAN, phone, street and door, exact coordinates,
// work session location) reaches a staff response only through a *Grant,
// and a grant exists only after Gate.Open has written its pii_access_log
// row in the reading transaction: no row, no grant, a masked answer.
//
// The type system holds the rule. A personal value read from the
// database is a Value; its only way out in clear is Reveal, which needs a
// grant for the value's field and data subject. A projection that fills a
// response from a Value therefore takes a *Grant parameter
// (internal/archtest TestRevealOnlyInProjections), and a nil grant gives
// the masked form. Grant has no exported field and no constructor but
// Open, so a grant cannot be made up.
//
// Faz 1 brings the gate and the rule; the personal fields themselves (TCKN,
// IBAN, addresses, locations) land with their domains from Faz 2 on.
package pii

import (
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Purpose says why staff open personal data, for the access log:
// "identity_review", "dispute_resolution". Lower snake case.
type Purpose string

// Field is a kind of personal data a grant opens.
type Field string

// Fields of personal data (plan §5.5).
const (
	FieldEmail        Field = "email"
	FieldPhone        Field = "phone"
	FieldTCKN         Field = "tckn"
	FieldIBAN         Field = "iban"
	FieldAddress      Field = "address"
	FieldCoordinates  Field = "coordinates"
	FieldWorkLocation Field = "work_location"
)

// knownFields are the fields a grant may open.
func knownFields() []Field {
	return []Field{FieldEmail, FieldPhone, FieldTCKN, FieldIBAN, FieldAddress, FieldCoordinates, FieldWorkLocation}
}

var purposePattern = regexp.MustCompile(`^[a-z][a-z_]{0,62}$`)

// Valid reports whether p is a purpose the access log accepts.
func (p Purpose) Valid() bool { return purposePattern.MatchString(string(p)) }

// Grant is the right to see some personal fields of one data subject,
// for one staff member, recorded in pii_access_log. Only Gate.Open makes
// one; the zero Grant and a nil *Grant open nothing.
type Grant struct {
	id      uuid.UUID
	staff   uuid.UUID
	subject uuid.UUID
	fields  []Field
}

// ID is the grant's pii_access_log row.
func (g *Grant) ID() uuid.UUID {
	if g == nil {
		return uuid.Nil
	}
	return g.id
}

// Allows reports whether g opens field f of the data subject.
func (g *Grant) Allows(f Field, subject uuid.UUID) bool {
	return g != nil && g.id != uuid.Nil && g.subject == subject && slices.Contains(g.fields, f)
}

// Value is a personal value of one field of one data subject, as read from
// the database. It has no exported field: its clear form comes out only
// through Reveal.
type Value[T any] struct {
	v       T
	field   Field
	subject uuid.UUID
}

// NewValue wraps the value of field f of subject, in the store that reads
// it.
func NewValue[T any](f Field, subject uuid.UUID, v T) Value[T] {
	return Value[T]{v: v, field: f, subject: subject}
}

// Reveal returns v in clear when g opens its field for its data subject;
// otherwise the zero T and false, and the projection writes the masked
// form. Call it only from a projection that takes the *Grant as a
// parameter (TestRevealOnlyInProjections).
func Reveal[T any](g *Grant, v Value[T]) (T, bool) {
	if !g.Allows(v.field, v.subject) {
		var zero T
		return zero, false
	}
	return v.v, true
}

// MaskString is the masked form of a string value, what a projection
// writes without a grant: every character but the last keep is "*" (keep
// is at most a quarter of the value, so a short value is masked whole).
func MaskString(v Value[string], keep int) string {
	r := []rune(v.v)
	keep = min(max(keep, 0), len(r)/4)
	return strings.Repeat("*", len(r)-keep) + string(r[len(r)-keep:])
}
