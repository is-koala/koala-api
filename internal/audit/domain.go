// Package audit is the staff audit trail core (plan §5.5, ADR-0026; Java
// admin_audit_entries, N01 §4.4).
//
// The trail is append-only evidence (M1). A mutation writes a PENDING row
// before it acts and a result row (SUCCEEDED or FAILED) naming the pending
// row after it; if the pending row cannot be written the mutation does not
// run. A refusal is one DENIED row; a sensitive read is one SUCCEEDED or
// FAILED row. Each row carries the actor, the basis of the authorization
// decision, the target, the action, the request id and the outcome; the
// personal part (masked body ≤ 8 KB, client address) is a separate
// detail row kept 6 months. The database computes the retention (24
// months, FINANCIAL 10 years), forbids UPDATE and DELETE by trigger, and
// lets koala_maint alone delete rows past their retention (Purge).
package audit

import (
	"github.com/is-koala/koala-api/internal/authz"
)

// Outcome is the outcome column of an entry.
type Outcome string

// Outcomes.
const (
	OutcomePending   Outcome = "PENDING"
	OutcomeSucceeded Outcome = "SUCCEEDED"
	OutcomeFailed    Outcome = "FAILED"
	OutcomeDenied    Outcome = "DENIED"
)

// MaxBodyBytes bounds the masked body of a detail row (Java: 8 KB). A
// larger body is replaced by a marker with its size.
const MaxBodyBytes = 8192

// Entry is what an audit row says, apart from its outcome.
type Entry struct {
	// Actor is a staff member or the system (the `koala staff` command).
	Actor authz.Subject
	// Action names what was done: the operationId of an admin operation
	// ("listStaffPermissions") or a dotted action ("staff.member.create").
	Action string
	// Permission is the staff permission the action needed; empty when
	// none did (the system acts on its own authority).
	Permission authz.Permission
	// Basis is why the actor was allowed; empty for a refusal or the
	// system.
	Basis authz.Basis
	// Target is the object acted on; the zero Object when there is none.
	Target authz.Object
	// RequestID ties the row to the request log.
	RequestID string
	// Mutating marks a change of state: it gets a pending row first.
	Mutating bool
	// Sensitivity decides the retention: FINANCIAL rows are kept 10
	// years, the rest 24 months.
	Sensitivity authz.Sensitivity
}

// piiKeyParts are the parts of a body member's key that make its value
// personal data or a secret: a key that contains one ("contactEmail",
// "user_phone", "ibanNumber", "refreshToken") is masked. Keys compare
// without case and without "_" and "-". Turkish spellings sit next to
// the English ones: a body written by the panel may use either.
func piiKeyParts() []string {
	return []string{
		"email", "phone", "mobile", "gsm", "tckn", "nationalid", "identitynumber", "kimlik", "iban", "password", "passwd",
		"secret", "token", "authorization", "apikey", "otp", "address", "street", "postcode", "zipcode",
		"latitude", "longitude", "coordinate", "location", "birth", "dogum",
		"firstname", "lastname", "fullname", "surname", "soyad", "ipaddress", "clientip",
		"cardnumber", "cardholder", "cardno", "creditcard", "cvv", "cvc",
	}
}

// piiKeySuffixes are key endings that mark personal data only at the end
// ("lat", "lng" would match too much inside a key).
func piiKeySuffixes() []string { return []string{"lat", "lng", "lon", "name"} }

// piiKeyExceptions end like a personal key but are not personal: names of
// things, not of people, and two flags of the staff contract that end in
// a personal part ("mustChangePassword", "tokenVersion"). They are
// matched as endings too, so "targetRoleName" and "memberTokenVersion"
// are exempt like the bare keys.
func piiKeyExceptions() []string {
	return []string{
		"rolename", "permissionname", "operationname", "filename", "categoryname", "badgename", "companyname",
		"tablename", "columnname", "fieldname", "schemaname", "indexname", "constraintname", "triggername",
		"eventname", "statusname", "actionname", "jobname", "queuename", "routename", "methodname",
		"typename", "tagname", "templatename", "hostname",
		"mustchangepassword", "tokenversion",
	}
}

// Masked is the value a masked body member gets.
const Masked = "***"
