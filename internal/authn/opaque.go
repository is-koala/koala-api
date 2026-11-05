package authn

import (
	"crypto/hmac"
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

// newOpaque returns a new random token value: the first refresh token
// of a session.
func newOpaque() string {
	var b [opaqueBytes]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+)
	return base64.RawURLEncoding.EncodeToString(b[:])
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

// successorOf is the token a rotation of token issues: HMAC-SHA256 keyed
// by the token over the salt stored with it at consumption. Only a holder
// of the token can compute it, and a retry inside the grace window gets
// the same successor back (ADR-0025 §3).
func successorOf(token string, salt []byte) string {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write(salt) // a hash.Hash never fails to write
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// newSalt returns the salt of a rotation.
func newSalt() []byte {
	b := make([]byte, opaqueBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error (Go 1.24+)
	return b
}

// targets keys a sign-in identifier with the server's login pepper: the
// login_attempts target and the rate limit key are HMAC-SHA256 of the
// normalized identifier, so neither stores an address and a leaked table
// cannot be matched against a list of addresses (ADR-0025 §5).
type targets struct {
	pepper []byte
}

func (t targets) of(normalized string) []byte {
	mac := hmac.New(sha256.New, t.pepper)
	_, _ = mac.Write([]byte(normalized)) // a hash.Hash never fails to write
	return mac.Sum(nil)
}

// normalizeEmail is the login identifier's canonical form (users.email
// and staff_members.email CHECKs).
func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}
