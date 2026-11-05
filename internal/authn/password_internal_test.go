package authn

import "testing"

// The dummy an unknown account is verified against is a real hash under
// the current parameters, so that branch costs what a known account
// costs (AUTH-001, AUTH-010).
func TestDummyHashCostsTheSame(t *testing.T) {
	t.Parallel()

	h, err := NewPasswords(DefaultParams(), 1)
	if err != nil {
		t.Fatal(err)
	}
	params, _, _, err := decode(h.dummy)
	if err != nil {
		t.Fatal(err)
	}
	if params != h.params {
		t.Fatalf("dummy parameters = %+v, current = %+v", params, h.params)
	}
}
