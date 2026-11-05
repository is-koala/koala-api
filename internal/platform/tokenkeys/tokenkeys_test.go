package tokenkeys_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
)

// seed returns a fresh random seed in the variable's encoding; test keys
// exist only in tests.
func seed(t *testing.T) string {
	t.Helper()
	b := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestParseRing(t *testing.T) {
	t.Parallel()

	a, b := seed(t), seed(t)
	r, err := tokenkeys.Parse("mp-2026-11:" + a + ", mp-2026-10:" + b)
	if err != nil {
		t.Fatal(err)
	}
	signer, ok := r.Signer()
	if !ok || signer.ID() != "mp-2026-11" {
		t.Fatalf("signer = %q, %v; want the first key", signer.ID(), ok)
	}
	for _, kid := range []string{"mp-2026-11", "mp-2026-10"} {
		if _, ok := r.Verifier(kid); !ok {
			t.Errorf("Verifier(%s) not found", kid)
		}
	}
	if _, ok := r.Verifier("other"); ok {
		t.Error("Verifier(other) found")
	}
	pub, _ := r.Verifier("mp-2026-11")
	if !pub.Equal(signer.Public()) {
		t.Error("verifier key differs from the signer's public key")
	}
}

func TestParseRingRejects(t *testing.T) {
	t.Parallel()

	s := seed(t)
	short := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	cases := map[string]string{
		"empty":         "",
		"no separator":  s,
		"bad kid":       "kid with space:" + s,
		"short seed":    "k1:" + short,
		"not base64url": "k1:" + strings.Repeat("!", 43),
		"padded":        "k1:" + base64.URLEncoding.EncodeToString(make([]byte, 32)),
		"repeated kid":  "k1:" + s + ",k1:" + seed(t),
		"too many":      "a:" + s + ",b:" + seed(t) + ",c:" + seed(t) + ",d:" + seed(t) + ",e:" + seed(t),
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := tokenkeys.Parse(spec)
			if err == nil {
				t.Fatal("Parse() succeeded")
			}
			if strings.Contains(err.Error(), s) {
				t.Errorf("error %q echoes the seed", err)
			}
		})
	}
}

func TestSharesKey(t *testing.T) {
	t.Parallel()

	a, b := seed(t), seed(t)
	mp, err := tokenkeys.Parse("mp:" + a)
	if err != nil {
		t.Fatal(err)
	}
	staff, err := tokenkeys.Parse("st:" + b)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := tokenkeys.Parse("st:" + b + ",st-old:" + a)
	if err != nil {
		t.Fatal(err)
	}
	if mp.SharesKey(staff) {
		t.Error("distinct rings share a key")
	}
	if !mp.SharesKey(reused) {
		t.Error("rings with a common seed under another kid do not share a key")
	}
}

func TestGenerate(t *testing.T) {
	t.Parallel()

	a, b := tokenkeys.Generate("mp"), tokenkeys.Generate("mp")
	if a.Empty() || a.SharesKey(b) {
		t.Fatal("generated rings are empty or equal")
	}
	k, _ := a.Signer()
	if k.ID() != "mp-ephemeral" {
		t.Errorf("kid = %q", k.ID())
	}
	if !(tokenkeys.Ring{}).Empty() {
		t.Error("zero ring is not empty")
	}
}
