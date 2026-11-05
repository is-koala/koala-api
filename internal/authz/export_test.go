package authz

// ConcealPolicy exposes the concealment policy to the black-box tests.
func ConcealPolicy() map[string]Action { return concealPolicy() }
