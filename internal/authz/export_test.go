package authz

// ConcealPolicy exposes the concealment policy to the black-box tests.
func ConcealPolicy() map[string]Action { return concealPolicy() }

// RawScope builds a Scope without the constructors, so the tests can
// reach the states Validate rejects.
func RawScope(kind ScopeKind, p Predicate, ids []string) Scope {
	return Scope{kind: kind, predicate: p, ids: ids}
}
