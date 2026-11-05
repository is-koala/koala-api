package authn

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// Opaque refresh tokens (K12, plan §3.9): 32 random bytes, handed out as
// 43 characters of unpadded base64url and stored only as their SHA-256.
// The entropy makes a slow hash unnecessary; a leaked table yields no
// usable token. Lookups go by hash, so no secret is ever compared in Go.
const (
	opaqueBytes = 32
	opaqueLen   = 43
)

// newOpaque returns a new token and its hash.
func newOpaque() (string, []byte) {
	var b [opaqueBytes]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+)
	v := base64.RawURLEncoding.EncodeToString(b[:])
	return v, hashOpaque(v)
}

// hashOpaque is the stored form of a token value.
func hashOpaque(v string) []byte {
	h := sha256.Sum256([]byte(v))
	return h[:]
}

// wellFormedOpaque reports whether v could be a token this package issued.
func wellFormedOpaque(v string) bool {
	if len(v) != opaqueLen {
		return false
	}
	return strings.Trim(v, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") == ""
}

// targetHash is the SHA-256 of a normalized sign-in identifier: the
// login_attempts target and the rate limit key, so neither stores an
// address.
func targetHash(normalized string) []byte {
	h := sha256.Sum256([]byte(normalized))
	return h[:]
}

// normalizeEmail is the login identifier's canonical form (users.email
// and staff_members.email CHECKs).
func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}
