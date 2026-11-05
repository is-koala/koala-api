package authz

import (
	"context"
	"fmt"
	"strings"

	"github.com/is-koala/koala-api/internal/platform/db"
)

// Wildcard is the id of a wildcard tuple user: "user:*" means every user.
const Wildcard = "*"

// TupleUser is the user side of a tuple, in OpenFGA's three forms:
// "user:<id>" (Relation empty), "user:*" (ID is Wildcard) and the userset
// "company:<id>#member" (Relation set).
type TupleUser struct {
	Type     string
	ID       string
	Relation string
}

// String is the tuple user in OpenFGA form.
func (u TupleUser) String() string {
	if u.Relation != "" {
		return u.Type + ":" + u.ID + "#" + u.Relation
	}
	return u.Type + ":" + u.ID
}

// IsWildcard reports whether u is "type:*".
func (u TupleUser) IsWildcard() bool { return u.ID == Wildcard && u.Relation == "" }

// IsObject reports whether u names one object ("type:id"), not a wildcard
// or a userset.
func (u TupleUser) IsObject() bool { return u.ID != Wildcard && u.Relation == "" }

// Tuple is a relationship tuple, object#relation@user, with the same
// fields and values as an OpenFGA tuple key (plan §5.2, research §6.2-3).
type Tuple struct {
	Object   Object
	Relation string
	User     TupleUser
}

// String is the tuple in "object#relation@user" form.
func (t Tuple) String() string {
	return t.Object.String() + "#" + t.Relation + "@" + t.User.String()
}

// ParseTuple parses "type:id#relation@user", where user is "type:id",
// "type:*" or "type:id#relation".
func ParseTuple(s string) (Tuple, error) {
	objRel, user, ok := strings.Cut(s, "@")
	if !ok {
		return Tuple{}, fmt.Errorf("authz: tuple %q: missing @user", s)
	}
	obj, rel, ok := strings.Cut(objRel, "#")
	if !ok || !ValidRelationName(rel) {
		return Tuple{}, fmt.Errorf("authz: tuple %q: missing or invalid #relation", s)
	}
	o, ok := ParseObject(obj)
	if !ok {
		return Tuple{}, fmt.Errorf("authz: tuple %q: object must be type:id", s)
	}
	u, err := ParseTupleUser(user)
	if err != nil {
		return Tuple{}, fmt.Errorf("authz: tuple %q: %w", s, err)
	}
	return Tuple{Object: o, Relation: rel, User: u}, nil
}

// ParseTupleUser parses "type:id", "type:*" or "type:id#relation", with
// OpenFGA's identifier rules.
func ParseTupleUser(s string) (TupleUser, error) {
	bad := fmt.Errorf("authz: tuple user %q: want type:id, type:* or type:id#relation", s)
	ref, rel, hasRel := strings.Cut(s, "#")
	t, id, ok := strings.Cut(ref, ":")
	if !ok {
		return TupleUser{}, bad
	}
	if id == Wildcard {
		if hasRel || !ValidTypeName(t) {
			return TupleUser{}, bad
		}
		return TupleUser{Type: t, ID: Wildcard}, nil
	}
	if !(Object{Type: t, ID: id}).Valid() || (hasRel && !ValidRelationName(rel)) {
		return TupleUser{}, bad
	}
	return TupleUser{Type: t, ID: id, Relation: rel}, nil
}

// TupleReader is where the evaluator reads relationship tuples. Its two
// methods mirror OpenFGA's storage reads (ReadUserTuple for an exact
// tuple; Read for the users of object#relation), so each maps to one small
// typed SQL query: EXISTS for Contains, a SELECT for Read.
//
// The in-memory source in authztest is for tests. Domain tables get one
// typed source per relation in Faz 2-3, reading in the caller's
// transaction; the authz_tuples view is not a TupleReader (plan §5.2-1).
type TupleReader interface {
	// Contains reports whether the tuple object#relation@user exists.
	// user is an object or a wildcard, never a userset.
	Contains(ctx context.Context, o Object, relation string, user TupleUser) (bool, error)
	// Read returns every user of object#relation: objects, wildcards and
	// usersets, in any order.
	Read(ctx context.Context, o Object, relation string) ([]TupleUser, error)
}

// ChangeOp is the kind of a RelationChange.
type ChangeOp uint8

// Change operations. The zero value is invalid.
const (
	// ChangeInvalid is the zero value; a sink rejects it.
	ChangeInvalid ChangeOp = iota
	// ChangeWrite adds the tuple.
	ChangeWrite
	// ChangeDelete removes the tuple.
	ChangeDelete
)

// RelationChange is the single domain event for every write that changes
// a relationship (plan §5.2-5, research §6.2-8): a domain store that
// changes a relation table emits one, in the same transaction.
type RelationChange struct {
	Op    ChangeOp
	Tuple Tuple
	// Actor is who caused the change, for the audit trail.
	Actor Subject
}

// RelationSink receives relationship changes in the writer's transaction.
// Today the planned sink writes authz_change_log (audit). On migration day
// it writes an outbox row that a relay applies to OpenFGA with idempotent
// writes (plan §5.7-1). The first implementation arrives with the first
// relation table (Faz 2).
type RelationSink interface {
	Record(ctx context.Context, q db.Querier, changes ...RelationChange) error
}
