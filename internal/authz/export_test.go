package authz

// ConcealPolicy exposes the concealment policy to the black-box tests.
func ConcealPolicy() map[string]Action { return concealPolicy() }

// RawScope builds a Scope without the constructors, so the tests can
// reach the states Validate rejects.
func RawScope(kind ScopeKind, p Predicate, ids []string) Scope {
	return Scope{kind: kind, predicate: p, ids: ids}
}

// LoadModelAllowingRecursionOn loads a model with the recursion rules of
// ADR-0028 relaxed for the named object types, as if they were in
// boundedDAGTypes. It is test-only on purpose, and it exists for one
// reason: the folder model of the conformance runner is built out of the
// two cases the rules now refuse (a recursive "from" chain on an
// unbounded type, and "but not" over a recursive relation), because that
// is where the Go evaluator and OpenFGA v1.22.0 disagree. The runner
// keeps proving that difference is still the documented one; production
// models go through LoadModel, which refuses it.
func LoadModelAllowingRecursionOn(data []byte, types []string) (*Model, error) {
	return loadModel(data, types)
}

// RecursiveRelations returns the "type#relation" names whose resolution
// can reach themselves, in sorted order, so a test can bind which
// relations of model.fga are recursive.
func (m *Model) RecursiveRelations() []string {
	var out []string
	for _, typ := range m.Types() {
		for _, rel := range m.Relations(typ) {
			if m.recursive(m.relation(typ, rel)) {
				out = append(out, typ+"#"+rel)
			}
		}
	}
	return out
}
