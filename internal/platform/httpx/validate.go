package httpx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/go-chi/chi/v5"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// Problem codes of request validation. Every code the service returns is
// listed in the contract's ErrorCode (api/components/problem.yaml); a test
// in internal/app checks the codes below against it.
const (
	CodeRequestInvalid = "REQUEST_INVALID"
	CodeBodyMalformed  = "REQUEST_BODY_MALFORMED"

	CodeFieldRequired      = "FIELD_REQUIRED"
	CodeFieldUnknown       = "FIELD_UNKNOWN"
	CodeFieldTypeInvalid   = "FIELD_TYPE_INVALID"
	CodeFieldFormatInvalid = "FIELD_FORMAT_INVALID"
	CodeFieldValueInvalid  = "FIELD_VALUE_INVALID"
	CodeFieldOutOfRange    = "FIELD_OUT_OF_RANGE"
)

// PlatformCodes lists every problem and field code this package returns.
func PlatformCodes() []apperr.Code {
	return []apperr.Code{
		apperr.CodeInternal, CodeRouteNotFound, CodeMethodNotAllowed, CodeBodyTooLarge,
		CodeRequestInvalid, CodeBodyMalformed,
		CodeFieldRequired, CodeFieldUnknown, CodeFieldTypeInvalid,
		CodeFieldFormatInvalid, CodeFieldValueInvalid, CodeFieldOutOfRange,
	}
}

// ValidateRequests returns middleware that validates a request against its
// operation in spec (kin-openapi, ADR-0006) before the generated handler
// binds it. It must run after chi has matched the route, so it is installed
// with Use on the group the generated routes are mounted on: the operation
// is looked up by the matched route pattern, which is the spec's path
// template verbatim.
//
// A mismatch is a 400 REQUEST_INVALID problem whose errors name each field
// (ProblemField.pointer: a JSON pointer into the body, or "<in>:<name>" for a
// parameter). Validator messages may contain request values, so they are
// never rendered or logged; only pointers and codes leave this function.
// The validated body value is kept on the request, and the generated
// handler binds that value instead of decoding the body a second time.
//
// Authentication is not checked here: it is the authn middleware's job.
func ValidateRequests(spec *openapi3.T, logger *slog.Logger) (func(http.Handler) http.Handler, error) {
	if spec == nil || spec.Paths == nil {
		return nil, errors.New("validate requests: spec has no paths")
	}
	routes := map[string]*routers.Route{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			routes[method+" "+path] = &routers.Route{
				Spec: spec, Path: path, PathItem: item, Method: method, Operation: op,
			}
		}
	}
	opts := &openapi3filter.Options{
		MultiError:         true,
		KeepValidatedValue: true,
		AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		// kin-openapi checks date, date-time, int32 and int64 by default;
		// uuid only when asked. Scoped to this validator, not kin's globals.
		SchemaValidationOptions: []openapi3.SchemaValidationOption{
			openapi3.WithStringFormatValidator("uuid",
				openapi3.NewRegexpFormatValidator(openapi3.FormatOfStringForUUIDOfRFC9562)),
		},
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rctx := chi.RouteContext(r.Context())
			if rctx == nil {
				WriteError(w, r, logger, errors.New("validate requests: no chi route context"))
				return
			}
			pattern := rctx.RoutePattern()
			route, ok := routes[r.Method+" "+pattern]
			if !ok {
				// A route without an operation is a wiring bug: fail closed.
				WriteError(w, r, logger, fmt.Errorf("validate requests: no operation for %s %s", r.Method, pattern))
				return
			}
			params := make(map[string]string, len(rctx.URLParams.Keys))
			for i, k := range rctx.URLParams.Keys {
				params[k] = rctx.URLParams.Values[i]
			}
			input := &openapi3filter.RequestValidationInput{
				Request: r, PathParams: params, Route: route, Options: opts,
			}
			if err := openapi3filter.ValidateRequest(r.Context(), input); err != nil {
				WriteError(w, r, logger, requestValidationError(err))
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// requestValidationError turns a kin-openapi request validation error into
// the client error: 413 when the body was over its limit, 400
// REQUEST_BODY_MALFORMED when it is not JSON, otherwise 400 REQUEST_INVALID
// with one field error per failing field.
func requestValidationError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return err // WriteError makes it 413 REQUEST_BODY_TOO_LARGE
	}
	var fields []apperr.FieldError
	malformed := false
	for _, re := range requestErrors(err, nil) {
		var parseErr *openapi3filter.ParseError
		switch {
		case re.Parameter != nil:
			fields = append(fields, parameterFields(re)...)
		case re.RequestBody != nil && errors.Is(re.Err, openapi3filter.ErrInvalidRequired):
			fields = append(fields, apperr.FieldError{Pointer: "", Code: CodeFieldRequired})
		case re.RequestBody != nil && re.Err == nil && strings.HasPrefix(re.Reason, "header Content-Type"):
			fields = append(fields, apperr.FieldError{Pointer: "header:Content-Type", Code: CodeFieldValueInvalid})
		case re.RequestBody != nil && errors.As(re.Err, &parseErr):
			malformed = true
		case re.RequestBody != nil:
			fields = append(fields, schemaFields(re.Err)...)
		default:
			fields = append(fields, apperr.FieldError{Pointer: "", Code: CodeFieldValueInvalid})
		}
	}
	if malformed && len(fields) == 0 {
		return apperr.Wrap(err, apperr.KindInvalid, CodeBodyMalformed)
	}
	slices.SortFunc(fields, func(a, b apperr.FieldError) int {
		if c := strings.Compare(a.Pointer, b.Pointer); c != 0 {
			return c
		}
		return strings.Compare(string(a.Code), string(b.Code))
	})
	return apperr.Wrap(err, apperr.KindInvalid, CodeRequestInvalid).WithFields(slices.Compact(fields)...)
}

func requestErrors(err error, out []*openapi3filter.RequestError) []*openapi3filter.RequestError {
	switch e := err.(type) { //nolint:errorlint // walks the error tree by its concrete nodes
	case openapi3.MultiError:
		for _, inner := range e {
			out = requestErrors(inner, out)
		}
		return out
	case *openapi3filter.RequestError:
		return append(out, e)
	}
	var re *openapi3filter.RequestError
	if errors.As(err, &re) {
		return append(out, re)
	}
	return append(out, &openapi3filter.RequestError{Err: err})
}

func parameterFields(re *openapi3filter.RequestError) []apperr.FieldError {
	pointer := re.Parameter.In + ":" + re.Parameter.Name
	if errors.Is(re.Err, openapi3filter.ErrInvalidRequired) {
		return []apperr.FieldError{{Pointer: pointer, Code: CodeFieldRequired}}
	}
	leaves := schemaLeaves(re.Err, nil)
	if len(leaves) == 0 {
		return []apperr.FieldError{{Pointer: pointer, Code: CodeFieldValueInvalid}}
	}
	out := make([]apperr.FieldError, 0, len(leaves))
	for _, se := range leaves {
		out = append(out, apperr.FieldError{Pointer: pointer, Code: keywordCode(se.SchemaField)})
	}
	return out
}

func schemaFields(err error) []apperr.FieldError {
	leaves := schemaLeaves(err, nil)
	if len(leaves) == 0 {
		return []apperr.FieldError{{Pointer: "", Code: CodeFieldValueInvalid}}
	}
	out := make([]apperr.FieldError, 0, len(leaves))
	for _, se := range leaves {
		base := jsonPointer(se.JSONPointer())
		code, names := memberError(se)
		if len(names) == 0 {
			out = append(out, apperr.FieldError{Pointer: base, Code: code})
			continue
		}
		for _, n := range names {
			out = append(out, apperr.FieldError{Pointer: base + "/" + escapePointer(n), Code: code})
		}
	}
	return out
}

// schemaLeaves flattens a schema error tree into the errors that name one
// failing keyword.
func schemaLeaves(err error, out []*openapi3.SchemaError) []*openapi3.SchemaError {
	switch e := err.(type) { //nolint:errorlint // walks the error tree by its concrete nodes
	case nil:
		return out
	case openapi3.MultiError:
		for _, inner := range e {
			out = schemaLeaves(inner, out)
		}
		return out
	case *openapi3.SchemaError:
		for origin := e.Origin; origin != nil; origin = errors.Unwrap(origin) {
			if me, ok := origin.(openapi3.MultiError); ok { //nolint:errorlint // see above
				return schemaLeaves(me, out)
			}
		}
		return append(out, e)
	}
	var inner *openapi3.SchemaError
	if errors.As(err, &inner) {
		return schemaLeaves(inner, out)
	}
	return out
}

func keywordCode(keyword string) apperr.Code {
	switch keyword {
	case "required":
		return CodeFieldRequired
	case "additionalProperties", "unevaluatedProperties":
		return CodeFieldUnknown
	case "type":
		return CodeFieldTypeInvalid
	case "format":
		return CodeFieldFormatInvalid
	case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum",
		"minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties":
		return CodeFieldOutOfRange
	default:
		return CodeFieldValueInvalid
	}
}

// memberError returns the field code of se and, when se is about members
// of an object rather than the object itself, the names of those members.
//
// kin-openapi validates an OpenAPI 3.1 schema with its JSON Schema 2020-12
// validator, but falls back to its OpenAPI 3.0 validator for schemas with
// internal $refs and no 2020-only keyword. The two name missing and unknown
// members differently: the 3.0 validator puts a missing member into the
// path and names an unknown one in its message (`property "x" is
// unsupported`); the 2020 validator reports both at the object and names
// them in its message (`missing properties 'a', 'b'`). The tests pin both
// formats, so a validator upgrade that changes them fails loudly.
func memberError(se *openapi3.SchemaError) (apperr.Code, []string) {
	switch se.SchemaField {
	case "required":
		return CodeFieldRequired, quotedNames(missingProps, se.Reason)
	case "additionalProperties", "unevaluatedProperties":
		return CodeFieldUnknown, quotedNames(extraProps, se.Reason)
	case "properties":
		if m := unsupportedProp.FindStringSubmatch(se.Reason); m != nil {
			if name, err := strconv.Unquote(m[1]); err == nil {
				return CodeFieldUnknown, []string{name}
			}
		}
	}
	return keywordCode(se.SchemaField), nil
}

var (
	missingProps    = regexp.MustCompile(`missing propert(?:y|ies) (.+)$`)
	extraProps      = regexp.MustCompile(`additional propert(?:y|ies) (.+) not allowed`)
	quotedName      = regexp.MustCompile(`'((?:[^'\\]|\\.)*)'`)
	unsupportedProp = regexp.MustCompile(`property ("(?:[^"\\]|\\.)*") is unsupported`)
)

func quotedNames(re *regexp.Regexp, reason string) []string {
	m := re.FindStringSubmatch(reason)
	if m == nil {
		return nil
	}
	var names []string
	for _, q := range quotedName.FindAllStringSubmatch(m[1], -1) {
		names = append(names, strings.ReplaceAll(q[1], `\'`, `'`))
	}
	return names
}

func jsonPointer(tokens []string) string {
	var sb strings.Builder
	for _, t := range tokens {
		sb.WriteByte('/')
		sb.WriteString(escapePointer(t))
	}
	return sb.String()
}

// escapePointer escapes one RFC 6901 reference token.
func escapePointer(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}
