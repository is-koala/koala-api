package authz

import (
	"context"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// SubjectKind tells which principal table a Subject comes from. The zero
// value is invalid, so a forgotten Subject is never a valid principal.
type SubjectKind uint8

// Subject kinds (plan §5.3).
const (
	// SubjectInvalid is the zero value; Check refuses it.
	SubjectInvalid SubjectKind = iota
	// SubjectUser is a marketplace user (users table, model type "user").
	SubjectUser
	// SubjectStaff is a staff member (staff_members, model type "staff").
	SubjectStaff
	// SubjectSystem is the service itself (River jobs, sweeps). It has no
	// model type: system work is authorized by its code path, not by
	// tuples, and Check refuses it.
	SubjectSystem
)

// Subject is the principal a decision is made for.
type Subject struct {
	Kind SubjectKind
	ID   uuid.UUID
}

// User returns the user subject with the given id.
func User(id uuid.UUID) Subject { return Subject{Kind: SubjectUser, ID: id} }

// Staff returns the staff subject with the given id.
func Staff(id uuid.UUID) Subject { return Subject{Kind: SubjectStaff, ID: id} }

// ModelType is the model type of the subject: "user" or "staff". It is
// empty for SubjectInvalid and SubjectSystem.
func (s Subject) ModelType() string {
	switch s.Kind {
	case SubjectUser:
		return TypeUser
	case SubjectStaff:
		return TypeStaff
	case SubjectInvalid, SubjectSystem:
		return ""
	}
	return ""
}

// String is the subject in OpenFGA user form, "user:<uuid>". It is empty
// when the subject has no model type.
func (s Subject) String() string {
	t := s.ModelType()
	if t == "" {
		return ""
	}
	return t + ":" + s.ID.String()
}

// OpenFGA's identifier rules (openfga/language validation rules), so that
// an id or name accepted here is accepted by OpenFGA too.
var (
	typeNamePattern     = regexp.MustCompile(`^[^:#@*\s]{1,254}$`)
	relationNamePattern = regexp.MustCompile(`^[^:#@*\s]{1,50}$`)
	objectIDPattern     = regexp.MustCompile(`^[^#:\s*][a-zA-Z0-9_|*@.+/\-]*$`)
	objectPattern       = regexp.MustCompile(`^\S{2,256}$`)
)

// ValidTypeName reports whether s is a valid OpenFGA type name.
func ValidTypeName(s string) bool { return typeNamePattern.MatchString(s) }

// ValidRelationName reports whether s is a valid OpenFGA relation name.
func ValidRelationName(s string) bool { return relationNamePattern.MatchString(s) }

// Object is a model object in OpenFGA form: Type "job_assignment", ID the
// row's id as a string.
type Object struct {
	Type string
	ID   string
}

// String is the object in OpenFGA form, "job_assignment:<id>".
func (o Object) String() string { return o.Type + ":" + o.ID }

// Valid reports whether o is a valid OpenFGA object: a valid type name, an
// id that follows OpenFGA's id rule (no wildcard), at most 256 characters.
func (o Object) Valid() bool {
	return ValidTypeName(o.Type) && objectIDPattern.MatchString(o.ID) &&
		o.ID != Wildcard && objectPattern.MatchString(o.String())
}

// ParseObject parses "type:id". It reports false when the result is not a
// valid object.
func ParseObject(s string) (Object, bool) {
	t, id, ok := strings.Cut(s, ":")
	o := Object{Type: t, ID: id}
	if !ok || !o.Valid() {
		return Object{}, false
	}
	return o, true
}

// Action is the name of a relation in model.fga, for example
// "can_complete". The object's type selects the type definition; the
// generated constants (Action<Type><Relation>, actions_gen.go) name every
// relation of the model.
type Action string

// ConsistencyToken is an opaque freshness token (a Zanzibar zookie,
// SpiceDB ZedToken). Today nothing produces one.
type ConsistencyToken string

// CheckOption adjusts a single check.
type CheckOption func(*checkOptions)

type checkOptions struct {
	atLeastAsFresh ConsistencyToken
}

// AtLeastAsFresh asks for a decision at least as fresh as token (plan
// §5.2-4). The local evaluator reads in the caller's transaction, so it
// already sees the caller's own writes and ignores the token; an external
// engine (SpiceDB at_least_as_fresh, OpenFGA HIGHER_CONSISTENCY) will not.
func AtLeastAsFresh(token ConsistencyToken) CheckOption {
	return func(o *checkOptions) { o.atLeastAsFresh = token }
}

// Decision is the outcome of a check.
type Decision struct {
	// Allowed reports whether the subject may perform the action.
	Allowed bool
	// Basis says why an allowed decision was allowed, for the audit log;
	// empty when denied. Basis.String is its audit form.
	Basis Basis
	// Conceal reports, for a denied decision, that the caller must answer
	// exactly as if the object did not exist: the resource's own 404,
	// byte for byte (ADR-0007, ADR-0014). False when allowed.
	Conceal bool
}

// Checker answers single and batched checks (plan §5.3).
type Checker interface {
	Check(ctx context.Context, s Subject, a Action, o Object, opts ...CheckOption) (Decision, error)
	CheckBatch(ctx context.Context, s Subject, a Action, objs []Object, opts ...CheckOption) (map[Object]Decision, error)
}

// Scoper computes the list filter for a subject, action and object type.
// Its implementation arrives with the first domain tables (Faz 2-3).
type Scoper interface {
	Scope(ctx context.Context, s Subject, a Action, objectType string) (Scope, error)
}

// CompanyCapabilities is the per-company part of the marketplace
// capabilities document.
type CompanyCapabilities struct {
	CompanyID uuid.UUID
	Role      string
	Can       map[string]bool
}

// Capabilities is the capabilities document (/v1/me/capabilities,
// /v1/admin/me/capabilities). Every value comes from Check (plan §5.5).
type Capabilities struct {
	Can         map[string]bool
	Companies   []CompanyCapabilities
	Permissions []string
}

// CapabilityReader builds the capabilities document of a subject. Its
// implementation arrives with the capabilities endpoints (Faz 2).
type CapabilityReader interface {
	Capabilities(ctx context.Context, s Subject) (Capabilities, error)
}
