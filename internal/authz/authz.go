package authz

import (
	"context"
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
		return "user"
	case SubjectStaff:
		return "staff"
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

// Object is a model object in OpenFGA form: Type "job_assignment", ID the
// row's id as a string.
type Object struct {
	Type string
	ID   string
}

// String is the object in OpenFGA form, "job_assignment:<id>".
func (o Object) String() string { return o.Type + ":" + o.ID }

// ParseObject parses "type:id". It reports false when either part is empty.
func ParseObject(s string) (Object, bool) {
	t, id, ok := strings.Cut(s, ":")
	if !ok || t == "" || id == "" {
		return Object{}, false
	}
	return Object{Type: t, ID: id}, true
}

// Action is the name of a relation in model.fga, for example
// "can_complete". The object's type selects the type definition.
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
	// Basis says why an allowed decision was allowed, for the audit log:
	// the chain of relations from the asked relation to the tuple that
	// granted it, for example
	// "job_assignment#can_read > job_assignment#party > job_assignment#worker".
	// A wildcard grant ends in " [user:*]". Empty when denied.
	Basis string
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

// ScopeKind is the state of a Scope.
type ScopeKind uint8

// Scope kinds. The zero value is ScopeNever, so a forgotten Scope lists
// nothing.
const (
	// ScopeNever: the subject can see no object of the type; the store
	// does not run the query.
	ScopeNever ScopeKind = iota
	// ScopeAlways: every object of the type.
	ScopeAlways
	// ScopeConditional: the objects matching Predicate, or the objects
	// whose ids are in IDs.
	ScopeConditional
)

// Predicate is a typed filter a store turns into its own WHERE clause; no
// SQL fragment is ever produced here (plan §5.5, ADR-0014).
type Predicate struct {
	SelfUserID *uuid.UUID
	CompanyIDs []uuid.UUID
}

// Scope is the three-state answer of a list filter (Cerbos query plan,
// plan §5.2-4). A Conditional scope carries either a Predicate (local SQL)
// or IDs (an external ListObjects result) so both fit one type.
type Scope struct {
	Kind      ScopeKind
	Predicate *Predicate
	IDs       []string
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
