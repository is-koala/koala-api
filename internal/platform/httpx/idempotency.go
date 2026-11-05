package httpx

import (
	"net/http"
	"strings"
)

// HeaderIdempotencyKey carries the client's key for a retried side effect
// (ADR-0005 madde 6).
const HeaderIdempotencyKey = "Idempotency-Key"

// Idempotency-Key length bounds (stil rehberi §7, öneri).
const (
	minIdempotencyKeyLen = 8
	maxIdempotencyKeyLen = 255
)

// IdempotencyKey returns the request's Idempotency-Key. It accepts both the
// bare value (Stripe's form) and the structured-field string of
// draft-ietf-httpapi-idempotency-key-header (`"abc"`); the quotes are
// stripped, so both name the same key. ok is false when the header is
// absent or is not 8-255 characters of [A-Za-z0-9_.:-].
func IdempotencyKey(h http.Header) (key string, ok bool) {
	v := strings.TrimSpace(h.Get(HeaderIdempotencyKey))
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	if len(v) < minIdempotencyKeyLen || len(v) > maxIdempotencyKeyLen {
		return "", false
	}
	for _, c := range []byte(v) {
		allowed := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == ':'
		if !allowed {
			return "", false
		}
	}
	return v, true
}
