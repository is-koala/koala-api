package httpx

import (
	"net/http"
	"strings"
)

// HeaderIdempotencyKey carries the client's key for a retried side effect
// (ADR-0005 madde 6).
const HeaderIdempotencyKey = "Idempotency-Key"

// HeaderIdempotentReplayed marks a response the Idempotency-Key store
// replayed instead of running the operation again (Stripe's header;
// ADR-0034).
const HeaderIdempotentReplayed = "Idempotent-Replayed"

// Problem codes of the Idempotency-Key store (ADR-0005 madde 6, ADR-0034).
// They are universal: any operation that takes the key can answer them.
const (
	// CodeIdempotencyKeyReused: the key was first used with another
	// request (422).
	CodeIdempotencyKeyReused = "IDEMPOTENCY_KEY_REUSED"
	// CodeIdempotencyKeyInUse: a request with the key is still running
	// (409); the client retries later with the same key.
	CodeIdempotencyKeyInUse = "IDEMPOTENCY_KEY_IN_USE"
)

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
