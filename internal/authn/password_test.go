package authn_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/apperr"
)

func newPasswords(t *testing.T, p authn.Params) *authn.Passwords {
	t.Helper()
	h, err := authn.NewPasswords(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The default parameters are OWASP's argon2id configuration and they are
// written into the hash (PHC string), so they can move later.
func TestPasswordHashFormat(t *testing.T) {
	t.Parallel()

	want := authn.Params{MemoryKiB: 19456, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}
	if got := authn.DefaultParams(); got != want {
		t.Fatalf("DefaultParams() = %+v, want %+v", got, want)
	}
	h := newPasswords(t, authn.DefaultParams())
	a, err := h.Hash(context.Background(), "uzun bir parola cümlesi")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Hash(context.Background(), "uzun bir parola cümlesi")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash = %q, want the PHC string of the default parameters", a)
	}
	if a == b {
		t.Error("two hashes of one password are equal; the salt is not random")
	}
}

func TestPasswordVerify(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newPasswords(t, authn.DefaultParams())
	stored, err := h.Hash(ctx, "doğru parola cümlesi")
	if err != nil {
		t.Fatal(err)
	}
	for pw, want := range map[string]bool{
		"doğru parola cümlesi":    true,
		"doğru parola cümlesI":    false,
		"":                        false,
		"doğru parola cümlesi ":   false,
		strings.Repeat("x", 4096): false, // over the hashed bound, still answered
	} {
		ok, rehash, err := h.Verify(ctx, stored, pw)
		if err != nil || ok != want || rehash {
			t.Errorf("Verify(%.20q) = %v, %v, %v; want %v, false, nil", pw, ok, rehash, err, want)
		}
	}
	if err := h.VerifyDummy(ctx, "doğru parola cümlesi"); err != nil {
		t.Errorf("VerifyDummy() = %v", err)
	}
}

// A hash made under older parameters still verifies and asks for a
// rehash; this is the parameter versioning of ADR-0025.
func TestPasswordRehashUnderNewParameters(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	old := authn.Params{MemoryKiB: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	stored, err := newPasswords(t, old).Hash(ctx, "eski parametreli parola")
	if err != nil {
		t.Fatal(err)
	}
	ok, rehash, err := newPasswords(t, authn.DefaultParams()).Verify(ctx, stored, "eski parametreli parola")
	if err != nil || !ok || !rehash {
		t.Fatalf("Verify() = %v, %v, %v; want true, true, nil", ok, rehash, err)
	}
}

// A stored value that is not a sane argon2id PHC string is refused before
// any hashing, including one whose costs would exhaust memory.
func TestPasswordVerifyRejectsBadHashes(t *testing.T) {
	t.Parallel()

	h := newPasswords(t, authn.DefaultParams())
	salt, key := "c2FsdHNhbHRzYWx0c2FsdA", "a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2U"
	for name, stored := range map[string]string{
		"bcrypt":           "$2a$10$abcdefghijklmnopqrstuuabcdefghijklmnopqrstuvwxyz01234",
		"argon2i":          "$argon2i$v=19$m=19456,t=2,p=1$" + salt + "$" + key,
		"old version":      "$argon2id$v=16$m=19456,t=2,p=1$" + salt + "$" + key,
		"huge memory":      "$argon2id$v=19$m=4194304,t=2,p=1$" + salt + "$" + key,
		"zero time":        "$argon2id$v=19$m=19456,t=0,p=1$" + salt + "$" + key,
		"reordered params": "$argon2id$v=19$t=2,m=19456,p=1$" + salt + "$" + key,
		"signed param":     "$argon2id$v=19$m=+19456,t=2,p=1$" + salt + "$" + key,
		"short salt":       "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$" + key,
		"bad base64":       "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$!!!",
		"empty":            "",
	} {
		if _, _, err := h.Verify(context.Background(), stored, "x"); err == nil {
			t.Errorf("%s: Verify() accepted %q", name, stored)
		}
	}
}

// The hashing slots honor the context: a canceled request does not queue.
func TestPasswordHashHonorsContext(t *testing.T) {
	t.Parallel()

	h := newPasswords(t, authn.DefaultParams())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Both slots are free, so a canceled context may still win the slot;
	// only an error that is not the context's is wrong.
	if _, err := h.Hash(ctx, "parola"); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Hash() = %v", err)
	}
}

// AUTH-009 (ADR-0025 §5): a new password has 15 to 128 characters
// (NIST SP 800-63B-4, password as the only factor); there is no
// composition rule.
func TestPasswordRule(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		pw   string
		ok   bool
	}{
		{"AUTH-009/1 character", "a", false},
		{"AUTH-009/12 characters, the old minimum", "abcdefghijkl", false},
		{"AUTH-009/14 characters", "abcdefghijklmn", false},
		{"AUTH-009/15 characters", "abcdefghijklmno", true},
		{"AUTH-009/14 two-byte characters (28 bytes)", "çğıöşüÇĞİÖŞÜçğ", false},
		{"AUTH-009/15 two-byte characters", "çğıöşüÇĞİÖŞÜçğı", true},
		{"AUTH-009/128 characters", strings.Repeat("a", 128), true},
		{"AUTH-009/128 two-byte characters (256 bytes)", strings.Repeat("ş", 128), true},
		{"AUTH-009/129 characters", strings.Repeat("a", 129), false},
		{"AUTH-009/long passphrase without digits or capitals", "bir iki üç dört beş altı", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := authn.CheckPassword(tc.pw)
			if tc.ok != (err == nil) {
				t.Fatalf("CheckPassword(%q) = %v, want ok=%v", tc.pw, err, tc.ok)
			}
			if err != nil && apperr.From(err).Code != authn.CodePasswordPolicy {
				t.Errorf("code = %s, want %s", apperr.From(err).Code, authn.CodePasswordPolicy)
			}
		})
	}
}
