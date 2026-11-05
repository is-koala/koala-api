package authz_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authz"
)

func TestScopeConstructors(t *testing.T) {
	t.Parallel()
	self := uuid.MustParse("0e6f4a42-5b1c-4c39-8f0e-2d6a3b7c9e11")
	company := uuid.MustParse("6f1b0d7e-3c8a-4f5e-9a51-2b7c0e4d8a10")

	if s := (authz.Scope{}); s.Kind() != authz.ScopeNever || s.Validate() != nil {
		t.Errorf("zero Scope: %v, %v; want a valid Never", s.Kind(), s.Validate())
	}
	for name, s := range map[string]authz.Scope{
		"Never":                      authz.Never(),
		"empty predicate":            authz.Conditional(authz.Predicate{}),
		"empty company list":         authz.Conditional(authz.Predicate{CompanyIDs: []uuid.UUID{}}),
		"no ids":                     authz.ConditionalIDs(nil),
		"empty id list":              authz.ConditionalIDs([]string{}),
		"normalized empty raw scope": authz.RawScope(authz.ScopeConditional, authz.Predicate{}, nil).Normalize(),
	} {
		if s.Kind() != authz.ScopeNever || s.Validate() != nil {
			t.Errorf("%s: kind %v, %v; want a valid Never", name, s.Kind(), s.Validate())
		}
		if _, ok := s.Predicate(); ok {
			t.Errorf("%s: carries a predicate", name)
		}
		if _, ok := s.IDs(); ok {
			t.Errorf("%s: carries ids", name)
		}
	}
	if s := authz.Always(); s.Kind() != authz.ScopeAlways || s.Validate() != nil {
		t.Errorf("Always: %v, %v", s.Kind(), s.Validate())
	}

	callerSelf := self
	p := authz.Predicate{SelfUserID: &callerSelf, CompanyIDs: []uuid.UUID{company}}
	s := authz.Conditional(p)
	got, ok := s.Predicate()
	if s.Kind() != authz.ScopeConditional || !ok || s.Validate() != nil || *got.SelfUserID != self || !slices.Equal(got.CompanyIDs, p.CompanyIDs) {
		t.Fatalf("Conditional: %+v, %v", got, ok)
	}
	// The scope owns its copy: changing the caller's predicate or the
	// returned one does not change the scope.
	p.CompanyIDs[0], got.CompanyIDs[0] = uuid.Nil, uuid.Nil
	*p.SelfUserID = uuid.Nil
	if again, _ := s.Predicate(); again.CompanyIDs[0] != company || *again.SelfUserID != self {
		t.Errorf("the scope's predicate changed through an alias: %+v", again)
	}
	if _, ok := s.IDs(); ok {
		t.Errorf("a predicate scope carries ids")
	}

	ids := authz.ConditionalIDs([]string{"b", "a", "b"})
	if list, ok := ids.IDs(); ids.Kind() != authz.ScopeConditional || !ok || !slices.Equal(list, []string{"a", "b"}) || ids.Validate() != nil {
		t.Fatalf("ConditionalIDs: %v, %v", list, ok)
	}
	if _, ok := ids.Predicate(); ok {
		t.Errorf("an id scope carries a predicate")
	}
}

func TestScopeValidate(t *testing.T) {
	t.Parallel()
	self := uuid.MustParse("0e6f4a42-5b1c-4c39-8f0e-2d6a3b7c9e11")
	p := authz.Predicate{SelfUserID: &self}
	for name, s := range map[string]authz.Scope{
		"predicate and ids":      authz.RawScope(authz.ScopeConditional, p, []string{"a"}),
		"empty conditional":      authz.RawScope(authz.ScopeConditional, authz.Predicate{}, nil),
		"never with a predicate": authz.RawScope(authz.ScopeNever, p, nil),
		"always with ids":        authz.RawScope(authz.ScopeAlways, authz.Predicate{}, []string{"a"}),
		"unknown kind":           authz.RawScope(authz.ScopeKind(9), authz.Predicate{}, nil),
	} {
		if err := s.Validate(); !errors.Is(err, authz.ErrInvalidScope) {
			t.Errorf("%s: %v, want ErrInvalidScope", name, err)
		}
	}
}
