// Package apperr is the single error model of the service (ADR-0007).
//
// Services return *Error values; transport code maps them to RFC 9457
// problem documents in exactly one place (httpx.WriteError). Domain packages
// never define their own NotFound/Forbidden sentinels.
package apperr

import (
	"errors"
	"fmt"
)

// Kind classifies an error. Each kind maps to one HTTP status in httpx.
// The zero value is KindInternal so an unclassified error never leaks as a
// client error.
type Kind uint8

// Kinds, as listed in plan §3.4. Add a kind only together with its mapping
// in httpx and a test.
const (
	KindInternal Kind = iota
	KindInvalid
	KindUnauthenticated
	KindForbidden
	KindNotFound
	KindConflict
	KindRateLimited
	KindUpstream
	// KindPayloadTooLarge: the request body exceeds the route's limit
	// (proposal of this PR; not in plan §3.4's list).
	KindPayloadTooLarge
)

// String returns a stable, lowercase name for logs.
func (k Kind) String() string {
	switch k {
	case KindInternal:
		return "internal"
	case KindInvalid:
		return "invalid"
	case KindUnauthenticated:
		return "unauthenticated"
	case KindForbidden:
		return "forbidden"
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindRateLimited:
		return "rate_limited"
	case KindUpstream:
		return "upstream"
	case KindPayloadTooLarge:
		return "payload_too_large"
	default:
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
}

// Code is a stable, machine-readable UPPER_SNAKE identifier such as
// PAYMENT_ATTEMPT_ACTIVE. Clients switch on it; it must never change meaning.
type Code string

// CodeInternal is the code of every error that is not an *Error.
const CodeInternal Code = "INTERNAL"

// FieldError points at one invalid request field (RFC 6901 JSON pointer).
type FieldError struct {
	Pointer string
	Code    Code
}

// Error is the application error. Cause is for logs only and is never
// rendered to clients.
type Error struct {
	Kind   Kind
	Code   Code
	Fields []FieldError
	cause  error
}

// New returns an error of the given kind and code.
func New(kind Kind, code Code) *Error {
	return &Error{Kind: kind, Code: code}
}

// Wrap returns an error of the given kind and code that keeps cause for
// logging and errors.Is/As.
func Wrap(cause error, kind Kind, code Code) *Error {
	return &Error{Kind: kind, Code: code, cause: cause}
}

// WithFields returns a copy of e carrying the given field errors.
func (e *Error) WithFields(fields ...FieldError) *Error {
	c := *e
	c.Fields = append(append([]FieldError(nil), e.Fields...), fields...)
	return &c
}

// Error implements error. It is safe on a nil receiver.
func (e *Error) Error() string {
	if e == nil {
		return "<nil *apperr.Error>"
	}
	if e.cause != nil {
		return fmt.Sprintf("%s %s: %v", e.Kind, e.Code, e.cause)
	}
	return fmt.Sprintf("%s %s", e.Kind, e.Code)
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// From returns the first *Error in err's chain. Errors that carry no *Error,
// including a typed nil *Error, are reported as internal so that unknown
// failures never become 4xx.
func From(err error) *Error {
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e
	}
	return &Error{Kind: KindInternal, Code: CodeInternal, cause: err}
}
