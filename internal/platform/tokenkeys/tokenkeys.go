// Package tokenkeys parses the signing key rings of the access token
// classes (ADR-0025). A ring is the value of one KOALA_JWT_*_KEYS variable:
// comma-separated "kid:seed" entries, where seed is the 32-byte Ed25519
// seed in unpadded base64url. The first entry signs; every entry verifies,
// so a key is rotated by adding the new key, then moving it to the front,
// then dropping the old one once its last token has expired (15 minutes).
//
// Errors name the entry by position, never by value: the seeds are
// secrets.
package tokenkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// MaxKeys bounds a ring: one signer and a few retiring keys. Rotation
// needs two at a time; four leave room for an emergency replacement.
const MaxKeys = 4

// kidPattern is the key id: short, printable, safe in a JWT header.
var kidPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Key is one Ed25519 key of a ring.
type Key struct {
	id      string
	private ed25519.PrivateKey
}

// ID returns the key id written to the JWT header's kid.
func (k Key) ID() string { return k.id }

// Private returns the signing key.
func (k Key) Private() ed25519.PrivateKey { return k.private }

// Public returns the verification key.
func (k Key) Public() ed25519.PublicKey {
	pub, _ := k.private.Public().(ed25519.PublicKey)
	return pub
}

// Ring is the keys of one token class. The zero Ring has no key.
type Ring struct {
	keys []Key
}

// Parse reads a ring from its variable value.
func Parse(spec string) (Ring, error) {
	if strings.TrimSpace(spec) == "" {
		return Ring{}, errors.New("no key")
	}
	entries := strings.Split(spec, ",")
	if len(entries) > MaxKeys {
		return Ring{}, fmt.Errorf("%d keys, want at most %d", len(entries), MaxKeys)
	}
	var r Ring
	seen := map[string]bool{}
	for i, e := range entries {
		kid, seed, ok := strings.Cut(strings.TrimSpace(e), ":")
		if !ok || !kidPattern.MatchString(kid) {
			return Ring{}, fmt.Errorf("key %d: want kid:seed with kid matching %s", i+1, kidPattern)
		}
		if seen[kid] {
			return Ring{}, fmt.Errorf("key %d: kid repeated", i+1)
		}
		seen[kid] = true
		raw, err := base64.RawURLEncoding.DecodeString(seed)
		if err != nil || len(raw) != ed25519.SeedSize {
			return Ring{}, fmt.Errorf("key %d: seed must be %d bytes in unpadded base64url", i+1, ed25519.SeedSize)
		}
		if lowEntropy(raw) {
			return Ring{}, fmt.Errorf("key %d: seed is not random (too few distinct bytes); generate one with `koala keygen jwt`", i+1)
		}
		r.keys = append(r.keys, Key{id: kid, private: ed25519.NewKeyFromSeed(raw)})
	}
	return r, nil
}

// minDistinct is the fewest distinct byte values a 32-byte secret may
// have. Random bytes have about 30; fewer than 16 has a probability far
// below 2^-100 and means a hand-made value (all zero, a repeated
// pattern, a word).
const minDistinct = 16

// lowEntropy reports a secret that cannot have come from a random source.
func lowEntropy(b []byte) bool {
	var seen [256]bool
	distinct := 0
	for _, c := range b {
		if !seen[c] {
			seen[c] = true
			distinct++
		}
	}
	return distinct < minDistinct
}

// NewKeyEntry returns a ring entry "kid:seed" with a fresh random seed,
// ready to put into a KOALA_JWT_*_KEYS variable (`koala keygen jwt`).
func NewKeyEntry(kid string) (string, error) {
	if !kidPattern.MatchString(kid) {
		return "", fmt.Errorf("kid must match %s", kidPattern)
	}
	seed := make([]byte, ed25519.SeedSize)
	_, _ = rand.Read(seed) // crypto/rand.Read never returns an error (Go 1.24+)
	return kid + ":" + base64.RawURLEncoding.EncodeToString(seed), nil
}

// PepperBytes is the size of a pepper made by NewPepper and the least a
// configured one may have.
const PepperBytes = 32

// ParsePepper reads a server secret for keyed hashes (KOALA_LOGIN_PEPPER):
// unpadded base64url of at least PepperBytes random bytes.
func ParsePepper(v string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil || len(raw) < PepperBytes {
		return nil, fmt.Errorf("want at least %d bytes in unpadded base64url", PepperBytes)
	}
	if lowEntropy(raw) {
		return nil, errors.New("not random (too few distinct bytes); generate one with `koala keygen pepper`")
	}
	return raw, nil
}

// NewPepper returns a fresh pepper value (`koala keygen pepper`).
func NewPepper() string {
	b := make([]byte, PepperBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error (Go 1.24+)
	return base64.RawURLEncoding.EncodeToString(b)
}

// GeneratePepper returns random pepper bytes for a process that has none
// configured (outside staging and production).
func GeneratePepper() []byte {
	b := make([]byte, PepperBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error (Go 1.24+)
	return b
}

// Generate returns a ring of one random key named prefix + "-ephemeral".
// serve uses it outside staging and production when no ring is
// configured; tokens signed with it die with the process.
func Generate(prefix string) Ring {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		// crypto/rand never fails (Go 1.24+); a failure here is a broken
		// platform, not a condition to handle.
		panic(fmt.Sprintf("tokenkeys: generate key: %v", err))
	}
	return Ring{keys: []Key{{id: prefix + "-ephemeral", private: priv}}}
}

// Empty reports whether the ring has no key.
func (r Ring) Empty() bool { return len(r.keys) == 0 }

// Signer returns the key that signs new tokens: the first.
func (r Ring) Signer() (Key, bool) {
	if len(r.keys) == 0 {
		return Key{}, false
	}
	return r.keys[0], true
}

// Verifier returns the public key with id kid.
func (r Ring) Verifier(kid string) (ed25519.PublicKey, bool) {
	for _, k := range r.keys {
		if k.id == kid {
			return k.Public(), true
		}
	}
	return nil, false
}

// SharesKey reports whether two rings hold the same key material, which
// would let a token of one class verify as the other.
func (r Ring) SharesKey(o Ring) bool {
	for _, a := range r.keys {
		for _, b := range o.keys {
			if subtle.ConstantTimeCompare(a.private.Seed(), b.private.Seed()) == 1 {
				return true
			}
		}
	}
	return false
}
