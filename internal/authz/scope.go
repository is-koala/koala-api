package authz

import (
	"errors"
	"slices"

	"github.com/google/uuid"
)

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
	// ScopeConditional: the objects matching a Predicate, or the objects
	// whose ids are in a set.
	ScopeConditional
)

// ErrInvalidScope is returned by Scope.Validate.
var ErrInvalidScope = errors.New("authz: invalid scope")

// Predicate is a typed filter a store turns into its own WHERE clause; no
// SQL fragment is ever produced here (plan §5.5, ADR-0014).
//
// The fields are joined with OR: an object matches when it is related to
// the subject's own user id (SelfUserID; the store knows which columns:
// worker_id, creator_id, ...) or to any company in CompanyIDs. A Predicate
// with neither matches nothing.
type Predicate struct {
	SelfUserID *uuid.UUID
	CompanyIDs []uuid.UUID
}

// Empty reports whether p matches nothing.
func (p Predicate) Empty() bool { return p.SelfUserID == nil && len(p.CompanyIDs) == 0 }

func (p Predicate) clone() Predicate {
	out := Predicate{CompanyIDs: slices.Clone(p.CompanyIDs)}
	if p.SelfUserID != nil {
		id := *p.SelfUserID
		out.SelfUserID = &id
	}
	return out
}

// Scope is the three-state answer of a list filter (Cerbos query plan,
// plan §5.2-4). A Conditional scope carries exactly one of a Predicate
// (local SQL) or an id set (an external ListObjects result), never both:
// the constructors cannot build both, and a filter that needs both is a
// new kind, not an implicit union. Build one with Never, Always,
// Conditional or ConditionalIDs; the zero value is Never.
type Scope struct {
	kind      ScopeKind
	predicate Predicate
	ids       []string
}

// Never is the scope that lists nothing.
func Never() Scope { return Scope{} }

// Always is the scope that lists every object of the type.
func Always() Scope { return Scope{kind: ScopeAlways} }

// Conditional is the scope of the objects matching p. An empty p is Never.
func Conditional(p Predicate) Scope {
	return Scope{kind: ScopeConditional, predicate: p.clone()}.Normalize()
}

// ConditionalIDs is the scope of the objects whose ids are in ids
// (duplicates removed, sorted). No ids is Never.
func ConditionalIDs(ids []string) Scope {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	return Scope{kind: ScopeConditional, ids: slices.Compact(sorted)}.Normalize()
}

// Kind is the state of the scope.
func (s Scope) Kind() ScopeKind { return s.kind }

// Predicate returns the predicate of a Conditional scope that filters by
// predicate.
func (s Scope) Predicate() (Predicate, bool) {
	if s.kind != ScopeConditional || s.predicate.Empty() {
		return Predicate{}, false
	}
	return s.predicate.clone(), true
}

// IDs returns the id set of a Conditional scope that filters by ids.
func (s Scope) IDs() ([]string, bool) {
	if s.kind != ScopeConditional || len(s.ids) == 0 {
		return nil, false
	}
	return slices.Clone(s.ids), true
}

// Normalize turns a Conditional scope that matches nothing into Never.
func (s Scope) Normalize() Scope {
	if s.kind == ScopeConditional && s.predicate.Empty() && len(s.ids) == 0 {
		return Never()
	}
	return s
}

// Validate checks the scope's invariants: Never and Always carry no
// filter; Conditional carries exactly one non-empty filter.
func (s Scope) Validate() error {
	hasPredicate, hasIDs := !s.predicate.Empty(), len(s.ids) > 0
	switch s.kind {
	case ScopeNever, ScopeAlways:
		if hasPredicate || hasIDs {
			return errors.Join(ErrInvalidScope, errors.New("never and always carry no filter"))
		}
		return nil
	case ScopeConditional:
		if hasPredicate == hasIDs {
			return errors.Join(ErrInvalidScope, errors.New("conditional carries exactly one of a predicate or ids"))
		}
		return nil
	}
	return errors.Join(ErrInvalidScope, errors.New("unknown kind"))
}
