package httpx

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/santhosh-tekuri/jsonschema/v6"

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

// fields2020 maps each JSON Schema 2020 error kind to a field error at the
// member it is about. Formats are asserted here only to produce a Format
// error: in production kin-openapi checks formats before this validator.
func TestFields2020(t *testing.T) {
	t.Parallel()

	const schema = `{
		"type": "object",
		"required": ["a", "b"],
		"additionalProperties": false,
		"properties": {
			"a": {"type": "string"},
			"b": {"type": "integer", "minimum": 1, "maximum": 10},
			"s": {"type": "string", "maxLength": 2},
			"list": {"type": "array", "maxItems": 1},
			"at": {"type": "string", "format": "date-time"},
			"k": {"const": "K"}
		}
	}`
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schema))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource("https://api.iskoala.com/schemas/test", doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("https://api.iskoala.com/schemas/test")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		value string
		want  []apperr.FieldError
	}{
		{"required", `{}`, []apperr.FieldError{
			{Pointer: "/a", Code: CodeFieldRequired}, {Pointer: "/b", Code: CodeFieldRequired},
		}},
		{"additional properties", `{"a":"x","b":1,"x~y":1}`, []apperr.FieldError{
			{Pointer: "/x~0y", Code: CodeFieldUnknown},
		}},
		{"type", `{"a":1,"b":1}`, []apperr.FieldError{{Pointer: "/a", Code: CodeFieldTypeInvalid}}},
		{"format", `{"a":"x","b":1,"at":"yesterday"}`, []apperr.FieldError{{Pointer: "/at", Code: CodeFieldFormatInvalid}}},
		{"minimum", `{"a":"x","b":0}`, []apperr.FieldError{{Pointer: "/b", Code: CodeFieldOutOfRange}}},
		{"maximum", `{"a":"x","b":11}`, []apperr.FieldError{{Pointer: "/b", Code: CodeFieldOutOfRange}}},
		{"maxLength", `{"a":"x","b":1,"s":"abc"}`, []apperr.FieldError{{Pointer: "/s", Code: CodeFieldOutOfRange}}},
		{"maxItems", `{"a":"x","b":1,"list":[1,2]}`, []apperr.FieldError{{Pointer: "/list", Code: CodeFieldOutOfRange}}},
		{"const", `{"a":"x","b":1,"k":"L"}`, []apperr.FieldError{{Pointer: "/k", Code: CodeFieldValueInvalid}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v, err := jsonschema.UnmarshalJSON(strings.NewReader(tt.value))
			if err != nil {
				t.Fatal(err)
			}
			var verr *jsonschema.ValidationError
			if !errors.As(sch.Validate(v), &verr) {
				t.Fatal("no validation error")
			}
			if got := sortFields(fields2020(verr, nil)); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
