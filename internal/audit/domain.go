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

// The body mask is an allow list (ADR-0028): a member's value stays in
// the trail only when its key is in allowedKeys or matches a class in
// allowedKeyClasses; everything else is Masked, whatever it holds. Keys
// compare without case and without "_" and "-", so "targetId", "target_id"
// and "TARGET-ID" are the one key "targetid".
//
// Extending it: a key goes into allowedKeys, as one normalized entry, in
// the PR that brings the operation whose body carries it, next to the
// comment that says which class of value it is. The reviewer's question is
// "could this ever hold something a person wrote, or something about a
// person" - if yes, the key stays out and the trail keeps the shape of the
// body without its value. allowedKeyClasses is not the place for a single
// key: a class is a generalization and buys every future key that ends in
// it. Nothing is ever added to make a test pass.

// allowedKeys are the exact keys whose value a detail row keeps in clear.
// They are reviewed one by one, so a key here is kept even when it reads
// like a personal one: "mustChangePassword" and "tokenVersion" are a flag
// and a counter of the staff contract, not a password and not a token.
// The composed keys that used to slip through the old exception list
// ("passwordFieldName", "emailHostname", "secretTokenVersion") are not
// these keys, and nothing else lets them through.
func allowedKeys() []string {
	return []string{
		// What was done and to what kind of thing: enums and stable codes.
		"action", "actionname", "code", "codes", "decision", "errorcode", "event", "eventname",
		"kind", "operation", "operationid", "operationname", "outcome", "reasoncode",
		"sensitivity", "state", "status", "statusname", "type",
		// Roles, permissions and scopes: names of authorizations, not of people.
		"permission", "permissionname", "permissions", "role", "rolecode", "rolecodes",
		"rolename", "rolenames", "roles", "scope", "scopes",
		// Counters, sizes and paging. ("count", "total", "version" and
		// "enabled" are not here: they are classes below, and a key that
		// is exactly a class would be a second rule saying the same.)
		"bytes", "cursor", "limit", "offset", "page", "quantity", "sort", "truncated",
		// Money (ADR-0008): minor units and a currency, never a card.
		"amount", "currency", "fee", "rate",
		// Flags of the staff contract and of a command. ("enabled" is a
		// class below.)
		"active", "approved", "disabled", "dryrun", "force",
		"mustchangepassword", "mutating", "required", "verified",
		// Version counters. ("version" itself is a class below.)
		"tokenversion",
		// Times and durations, set by the database or the service.
		"createdat", "deletedat", "duration", "expiresat", "lastseenat", "occurredat",
		"retention", "retentionuntil", "revokedat", "timestamp", "ttl", "updatedat",
		// Names developers choose, not names of people: schema and routing
		// objects, and the taxonomies staff maintain.
		"badgename", "categoryname", "columnname", "constraintname", "fieldname", "indexname",
		"methodname", "queuename", "routename", "schemaname", "tablename", "tagname",
		"templatename", "triggername", "typename",
	}
}

// allowedKeyClasses are key endings that name the *shape* of a value, not
// its meaning: an identifier, a counter, a version, a boolean. A key
// matched by its class is kept only when it holds no part of piiKeyParts,
// so "nationalId", "emailId" and "passportId" are masked although they
// end in an allowed class.
//
// Meaning-bearing endings are deliberately not here. "status", "state",
// "type", "kind", "code", "codes" and "flag" were, and they reopened the
// hole the allow list exists to close: the only thing narrowing a class is
// piiKeyParts, which is a deny list, and the natural names of KVKK m.6
// special-category fields end in exactly those words
// ("disabilityStatus", "bloodType", "religionType", "diseaseCode",
// "smokerFlag"). A deny list cannot be relied on to name every one of
// them before Faz 2 writes one into an append-only trail. The keys that
// really need those endings today are in allowedKeys by name ("status",
// "statusname", "type", "kind", "code", "codes", "errorcode",
// "reasoncode", "rolecode"), which is a reviewed decision per key.
//
// A class ending stays short, generic and about shape. Adding one buys
// every future key that ends in it, so a single key goes in allowedKeys.
func allowedKeyClasses() []string {
	return []string{"count", "enabled", "id", "ids", "total", "uuid", "version"}
}

// piiKeyParts are the parts of a key that make its value personal data or
// a secret ("contactEmail", "user_phone", "ibanNumber", "refreshToken").
// Under the allow list they no longer decide what is masked - an unlisted
// key is masked whether it holds a part or not - they narrow
// allowedKeyClasses, which is the only generalizing rule left. Turkish
// spellings sit next to the English ones: a body written by the panel may
// use either.
//
// The special-category group (KVKK m.6: health, disability, religion,
// union membership, criminal record, sex life, biometrics, and the civil
// and military status Turkish forms ask for) is here because an
// identifier of such a record ("disabilityId", "healthRecordCount") would
// otherwise pass on its class alone. It is fail-closed on purpose: a few
// innocent keys lose their value ("healthCheckCount", "licenseCode" of a
// software license) and nothing of a data subject's reaches the trail.
//
// Two words were considered and left out because a part is matched
// anywhere in the key, with no word boundary: "din" (religion) also
// matches ordering, binding, holding, funding and pending, and "inanc"
// (belief) also matches financial. Their fields are covered by
// "religio", "mezhep", "sabika" and by the class pruning above.
func piiKeyParts() []string {
	return []string{
		"email", "phone", "mobile", "gsm", "tckn", "nationalid", "identitynumber", "kimlik", "iban", "password", "passwd",
		"secret", "token", "authorization", "apikey", "otp", "address", "adres", "street", "postcode", "zipcode",
		"latitude", "longitude", "coordinate", "location", "birth", "dogum",
		"firstname", "lastname", "fullname", "surname", "soyad", "ipaddress", "clientip",
		"cardnumber", "cardholder", "cardno", "creditcard", "cvv", "cvc", "doorcode",
		// KVKK m.6 and the identity documents that carry it.
		"health", "saglik", "disabilit", "engel", "handicap", "pregnan", "gebe",
		"religio", "mezhep", "union", "sendika", "criminal", "sabika", "sicil",
		"gender", "cinsiyet", "orientation", "biometric", "biyometri",
		"marital", "medenihal", "military", "askerlik",
		"blood", "kangrubu", "disease", "hastalik", "medication", "ilac", "allergy", "alerji",
		"smoker", "sigara", "salary", "maas", "sgk",
		"passport", "pasaport", "license", "ehliyet", "taxid",
	}
}

// Masked is the value a masked body member gets.
const Masked = "***"
