package httpx

import (
	"errors"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// What the validator cannot classify is a server error: 500, logged, and
// described without the request values its messages may carry.
func TestRequestValidationErrorUnknownIsServerError(t *testing.T) {
	t.Parallel()

	const secret = "TR330006100519786457841326"
	tests := map[string]error{
		"security": &openapi3filter.SecurityRequirementsError{Errors: []error{errors.New("token " + secret)}},
		"body error of an unknown type": &openapi3filter.RequestError{
			RequestBody: &openapi3.RequestBody{}, Err: errors.New("compile schema: " + secret),
		},
		"parameter error of an unknown type": &openapi3filter.RequestError{
			Parameter: &openapi3.Parameter{Name: "id", In: "path"}, Err: errors.New(secret),
		},
		"unsupported parameter format": &openapi3filter.RequestError{
			Parameter: &openapi3.Parameter{Name: "id", In: "path"},
			Err:       &openapi3filter.ParseError{Kind: openapi3filter.KindUnsupportedFormat, Reason: secret},
		},
	}
	for name, err := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := requestValidationError(openapi3.MultiError{err})
			if e := apperr.From(got); e.Kind != apperr.KindInternal {
				t.Fatalf("kind = %s, want internal (500)", e.Kind)
			}
			if strings.Contains(got.Error(), secret) {
				t.Fatalf("server error carries a request value: %v", got)
			}
		})
	}
}

// Client errors carry no cause: the validator's message stays out of the
// error chain entirely.
func TestRequestValidationErrorClientErrorsHaveNoCause(t *testing.T) {
	t.Parallel()

	const secret = "TR330006100519786457841326"
	err := &openapi3filter.RequestError{
		RequestBody: &openapi3.RequestBody{},
		Err:         &openapi3filter.ParseError{Kind: openapi3filter.KindInvalidFormat, Reason: secret},
	}
	got := requestValidationError(openapi3.MultiError{err})
	if e := apperr.From(got); e.Kind != apperr.KindInvalid || e.Code != CodeBodyMalformed {
		t.Fatalf("got %s %s", e.Kind, e.Code)
	}
	if errors.Unwrap(got) != nil || strings.Contains(got.Error(), secret) {
		t.Fatalf("client error keeps the validator's message: %v", got)
	}
}
