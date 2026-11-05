package authn

import (
	"context"
	"testing"
)

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

// A decoy verification of a stored hash pays the derivations a real one
// pays, the extra one of a hash under old parameters included; the dummy
// pays what a hash under current parameters pays (ADR-0025 §5–6).
func TestDecoyPaysTheDerivationsOfTheHash(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, err := NewPasswords(DefaultParams(), 1)
	if err != nil {
		t.Fatal(err)
	}
	old, err := NewPasswords(Params{MemoryKiB: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 1)
	if err != nil {
		t.Fatal(err)
	}
	const pw = "personelin uzun parolası"
	current, err := h.Hash(ctx, pw)
	if err != nil {
		t.Fatal(err)
	}
	oldHash, err := old.Hash(ctx, pw)
	if err != nil {
		t.Fatal(err)
	}
	count := func(f func() error) int64 {
		t.Helper()
		before := h.derivations.Load()
		if err := f(); err != nil {
			t.Fatal(err)
		}
		return h.derivations.Load() - before
	}
	verify := func(stored, p string) func() error {
		return func() error { _, _, err := h.Verify(ctx, stored, p); return err }
	}
	for _, tc := range []struct {
		name string
		got  int64
		want int64
	}{
		{"verify, current hash, right", count(verify(current, pw)), 1},
		{"verify, current hash, wrong", count(verify(current, "yanlış")), 1},
		{"decoy, current hash, right", count(func() error { return h.VerifyDecoy(ctx, current, pw) }), 1},
		{"verify, old hash, right", count(verify(oldHash, pw)), 2},
		{"verify, old hash, wrong", count(verify(oldHash, "yanlış")), 2},
		{"decoy, old hash, right", count(func() error { return h.VerifyDecoy(ctx, oldHash, pw) }), 2},
		{"decoy, old hash, wrong", count(func() error { return h.VerifyDecoy(ctx, oldHash, "yanlış") }), 2},
		{"dummy", count(func() error { return h.VerifyDummy(ctx, pw) }), 1},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: %d derivations, want %d", tc.name, tc.got, tc.want)
		}
	}
}
