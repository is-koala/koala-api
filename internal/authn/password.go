package authn

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are argon2id cost parameters (RFC 9106). They are written into
// every hash (PHC string format), so a hash keeps verifying after the
// defaults move, and Verify reports a hash made under other parameters
// for rehashing at the next successful sign-in.
type Params struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
	SaltLen   uint32
	KeyLen    uint32
}

// DefaultParams are OWASP's Password Storage Cheat Sheet configuration for
// argon2id: 19 MiB, 2 iterations, 1 lane (equivalent in strength to its
// 46 MiB / 1 iteration option and cheaper in memory per concurrent
// sign-in); a 16-byte salt and a 32-byte key (RFC 9106 §4).
func DefaultParams() Params {
	return Params{MemoryKiB: 19 * 1024, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}
}

// Bounds a stored hash's parameters must stay within. A hash is read from
// the database; one with absurd costs would be a way to exhaust memory.
const (
	maxMemoryKiB = 256 * 1024
	maxTime      = 10
	maxThreads   = 16
	minSaltLen   = 8
	maxSaltLen   = 64
	minKeyLen    = 16
	maxKeyLen    = 64
)

// maxPasswordBytes bounds what is hashed at sign-in, far above the rule's
// 72 bytes (the contract caps the field too).
const maxPasswordBytes = 1024

// errBadHash means a stored value is not an argon2id PHC string this
// package can verify.
var errBadHash = errors.New("authn: stored password hash is not a supported argon2id PHC string")

// Passwords hashes and verifies passwords. It bounds how many hashes run
// at once: each holds MemoryKiB of memory for tens of milliseconds.
type Passwords struct {
	params Params
	// dummy is a hash of a random password under params. An unknown
	// account is verified against it, so it pays the same cost as a known
	// one (AUTH-001, AUTH-010).
	dummy string
	slots chan struct{}
}

// NewPasswords returns a hasher with params, running at most concurrent
// hashes at a time.
func NewPasswords(params Params, concurrent int) (*Passwords, error) {
	if err := params.check(); err != nil {
		return nil, err
	}
	if concurrent < 1 {
		return nil, errors.New("authn: concurrent hashes must be at least 1")
	}
	p := &Passwords{params: params, slots: make(chan struct{}, concurrent)}
	var random [32]byte
	_, _ = rand.Read(random[:]) // crypto/rand.Read never returns an error (Go 1.24+)
	p.dummy = p.encode(base64.RawStdEncoding.EncodeToString(random[:]))
	return p, nil
}

func (p Params) check() error {
	switch {
	case p.MemoryKiB < 8*uint32(p.Threads) || p.MemoryKiB > maxMemoryKiB:
		return fmt.Errorf("authn: argon2id memory %d KiB out of range", p.MemoryKiB)
	case p.Time < 1 || p.Time > maxTime:
		return fmt.Errorf("authn: argon2id time %d out of range", p.Time)
	case p.Threads < 1 || p.Threads > maxThreads:
		return fmt.Errorf("authn: argon2id threads %d out of range", p.Threads)
	case p.SaltLen < minSaltLen || p.SaltLen > maxSaltLen:
		return fmt.Errorf("authn: salt length %d out of range", p.SaltLen)
	case p.KeyLen < minKeyLen || p.KeyLen > maxKeyLen:
		return fmt.Errorf("authn: key length %d out of range", p.KeyLen)
	}
	return nil
}

// Hash returns the PHC string of password under the current parameters.
func (h *Passwords) Hash(ctx context.Context, password string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	return h.encode(password), nil
}

// Verify reports whether password matches the stored hash and whether the
// hash was made under other parameters than the current ones (rehash it
// after a successful sign-in). The key comparison is constant time.
func (h *Passwords) Verify(ctx context.Context, stored, password string) (ok, rehash bool, err error) {
	params, salt, key, err := decode(stored)
	if err != nil {
		return false, false, err
	}
	if len(password) > maxPasswordBytes {
		// Still pay for one hash: a long password must not answer faster.
		_, _ = h.verifyKey(ctx, h.params, salt, key, "")
		return false, false, nil
	}
	ok, err = h.verifyKey(ctx, params, salt, key, password)
	if err != nil {
		return false, false, err
	}
	return ok, params != h.params, nil
}

// VerifyDummy pays the cost of one verification for an account that does
// not exist or has no password, and always fails.
func (h *Passwords) VerifyDummy(ctx context.Context, password string) error {
	_, _, err := h.Verify(ctx, h.dummy, password)
	return err
}

func (h *Passwords) verifyKey(ctx context.Context, params Params, salt, key []byte, password string) (bool, error) {
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(password), salt, params.Time, params.MemoryKiB, params.Threads, params.KeyLen)
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

func (h *Passwords) acquire(ctx context.Context) error {
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("authn: wait for a password hash slot: %w", ctx.Err())
	}
}

func (h *Passwords) release() { <-h.slots }

// encode hashes password with a fresh salt and formats the PHC string
// $argon2id$v=19$m=<KiB>,t=<iterations>,p=<lanes>$<salt>$<key>.
func (h *Passwords) encode(password string) string {
	salt := make([]byte, h.params.SaltLen)
	_, _ = rand.Read(salt) // crypto/rand.Read never returns an error (Go 1.24+)
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.MemoryKiB, h.params.Threads, h.params.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		h.params.MemoryKiB, h.params.Time, h.params.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// decode parses a PHC string and checks its parameters against the bounds.
func decode(stored string) (Params, []byte, []byte, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return Params{}, nil, nil, errBadHash
	}
	var p Params
	// Scanned straight into the typed fields; re-rendering the result must
	// give the input back, so no sign, padding or trailing text slips by.
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil || n != 3 ||
		fmt.Sprintf("m=%d,t=%d,p=%d", p.MemoryKiB, p.Time, p.Threads) != parts[3] {
		return Params{}, nil, nil, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, errBadHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, errBadHash
	}
	var okSalt, okKey bool
	p.SaltLen, okSalt = smallLen(len(salt), minSaltLen, maxSaltLen)
	p.KeyLen, okKey = smallLen(len(key), minKeyLen, maxKeyLen)
	if !okSalt || !okKey || p.check() != nil {
		return Params{}, nil, nil, errBadHash
	}
	return p, salt, key, nil
}

// smallLen returns n as a uint32 when it lies in [lo, hi]. The bounds are
// small; the search avoids a narrowing conversion of an unchecked int.
func smallLen(n int, lo, hi uint32) (uint32, bool) {
	for l := lo; l <= hi; l++ {
		if int(l) == n {
			return l, true
		}
	}
	return 0, false
}
