// Package authztest provides an in-memory authz.TupleReader for tests and
// for the conformance runner. Production code reads tuples from domain
// tables (Faz 2-3); this package is never wired into the service.
package authztest

import (
	"context"
	"fmt"

	"github.com/is-koala/koala-api/internal/authz"
)

// Tuples is an immutable in-memory tuple set.
type Tuples struct {
	byObjectRelation map[string][]authz.TupleUser
	all              []authz.Tuple
}

var _ authz.TupleReader = (*Tuples)(nil)

// NewTuples validates every tuple against the model, as an OpenFGA write
// would, and returns the set. Duplicates are an error, as in OpenFGA.
func NewTuples(m *authz.Model, tuples ...authz.Tuple) (*Tuples, error) {
	t := &Tuples{byObjectRelation: map[string][]authz.TupleUser{}}
	seen := map[string]bool{}
	for _, tp := range tuples {
		if err := m.ValidateTuple(tp); err != nil {
			return nil, fmt.Errorf("authztest: %w", err)
		}
		if seen[tp.String()] {
			return nil, fmt.Errorf("authztest: duplicate tuple %s", tp)
		}
		seen[tp.String()] = true
		k := key(tp.Object, tp.Relation)
		t.byObjectRelation[k] = append(t.byObjectRelation[k], tp.User)
		t.all = append(t.all, tp)
	}
	return t, nil
}

// All returns the tuples in insertion order.
func (t *Tuples) All() []authz.Tuple {
	return append([]authz.Tuple(nil), t.all...)
}

// Contains implements authz.TupleReader.
func (t *Tuples) Contains(_ context.Context, o authz.Object, relation string, user authz.TupleUser) (bool, error) {
	for _, u := range t.byObjectRelation[key(o, relation)] {
		if u == user {
			return true, nil
		}
	}
	return false, nil
}

// Read implements authz.TupleReader.
func (t *Tuples) Read(_ context.Context, o authz.Object, relation string) ([]authz.TupleUser, error) {
	return append([]authz.TupleUser(nil), t.byObjectRelation[key(o, relation)]...), nil
}

func key(o authz.Object, relation string) string { return o.String() + "#" + relation }
