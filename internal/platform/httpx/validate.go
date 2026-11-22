package httpx

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
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
// in internal/app checks PlatformCodes against it.
const (
	CodeRequestInvalid         = "REQUEST_INVALID"
	CodeBodyMalformed          = "REQUEST_BODY_MALFORMED"
	CodeContentTypeUnsupported = "CONTENT_TYPE_UNSUPPORTED"

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
		CodeRequestInvalid, CodeBodyMalformed, CodeContentTypeUnsupported,
		CodeFieldRequired, CodeFieldUnknown, CodeFieldTypeInvalid,
		CodeFieldFormatInvalid, CodeFieldValueInvalid, CodeFieldOutOfRange,
		CodePathInvalid, CodeAppVersionUnsupported, CodeRateLimited,
	}
}

// ExtBodyLimit is the operation extension that sets the operation's
// request body limit in bytes, replacing DefaultBodyLimit.
const ExtBodyLimit = "x-body-limit"

// errValidatedBody is what reading a validated request body returns: the
// handler binds the validated value instead (see ValidateRequests).
var errValidatedBody = errors.New("httpx: the request body was validated; bind its validated value")

type validatedRoute struct {
	route     *routers.Route
	bodyLimit int64         // 0: the router's limit stays
	body2020  *compiled2020 // nil: no JSON Schema 2020-only keyword in the body
}

// ValidateRequests returns middleware that validates a request against its
// operation in spec (kin-openapi, ADR-0019) before the generated handler
// binds it.
//
// Order. It runs after chi has matched the route: it is installed with Use
// on the group the generated routes are mounted on, and finds the operation
// by the matched route pattern, which is the spec's path template verbatim.
// Authentication runs before it, so an unauthenticated caller learns
// nothing about the request shape; it does not check security itself.
//
// Errors. A client mistake is a problem document without the validator's
// messages, which may contain request values: 400 REQUEST_INVALID with one
// field error per failing member or parameter, 400 REQUEST_BODY_MALFORMED,
// 413 REQUEST_BODY_TOO_LARGE or 415 CONTENT_TYPE_UNSUPPORTED. Anything it
// cannot classify (a schema that does not compile, a security error, a new
// error type) is a 500 and is logged without request values.
//
// Body. The operation's x-body-limit, if any, replaces the router's limit
// before the body is read. kin-openapi reads the whole body, decodes it,
// writes schema defaults into it and replaces r.Body in place. Afterwards
// r.Body only carries the validated value (ValidatedValue() any), which the
// generated handler binds; reading it fails, so nothing decodes the raw
// bytes a second time. Code that needs the body after this middleware
// (the idempotency fingerprint) hashes the validated value, not raw bytes.
//
// Validator. kin-openapi's JSON Schema 2020-12 validator, which it uses for
// OpenAPI 3.1 documents, compiles the schema on every request; it has no
// cache. Its OpenAPI 3.0 validator walks the schema without compiling and
// checks every keyword except the 2020-only ones (const,
// unevaluatedProperties, if/then/else, …); kin itself falls back to it for
// schemas with $refs. So every request goes through the 3.0 validator, and
// an operation whose body uses a 2020-only keyword is checked a second
// time against its schema compiled once at start-up (jsonschema/v6, the
// compiler kin uses; ADR-0019 Karar 2). A parameter schema with a
// 2020-only keyword is refused at start-up.
func ValidateRequests(spec *openapi3.T, logger *slog.Logger) (func(http.Handler) http.Handler, error) {
	if spec == nil || spec.Paths == nil {
		return nil, errors.New("validate requests: spec has no paths")
	}
	// A shallow copy that only differs in the version: openapi3filter picks
	// its validator from it and from nothing else.
	walked := *spec
	walked.OpenAPI = "3.0.3"

	routes := map[string]validatedRoute{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			limit, err := bodyLimit(op)
			if err != nil {
				return nil, fmt.Errorf("validate requests: %s %s: %w", method, path, err)
			}
			body2020, err := compileBody2020(method+" "+path, item, op)
			if err != nil {
				return nil, fmt.Errorf("validate requests: %s %s: %w", method, path, err)
			}
			routes[method+" "+path] = validatedRoute{
				route:     &routers.Route{Spec: &walked, Path: path, PathItem: item, Method: method, Operation: op},
				bodyLimit: limit,
				body2020:  body2020,
			}
		}
	}
	opts := &openapi3filter.Options{
		MultiError:                        true,
		KeepValidatedValue:                true,
		RejectWhenRequestBodyNotSpecified: true,
		AuthenticationFunc:                openapi3filter.NoopAuthenticationFunc,
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
			vr, ok := routes[r.Method+" "+pattern]
			if !ok {
				// A route without an operation is a wiring bug: fail closed.
				WriteError(w, r, logger, fmt.Errorf("validate requests: no operation for %s %s", r.Method, pattern))
				return
			}
			if vr.bodyLimit > 0 {
				r = limitBody(w, r, vr.bodyLimit)
			}
			params := make(map[string]string, len(rctx.URLParams.Keys))
			for i, k := range rctx.URLParams.Keys {
				params[k] = rctx.URLParams.Values[i]
			}
			input := &openapi3filter.RequestValidationInput{
				Request: r, PathParams: params, Route: vr.route, Options: opts,
			}
			if err := openapi3filter.ValidateRequest(r.Context(), input); err != nil {
				WriteError(w, r, logger, requestValidationError(err))
				return
			}
			if b, ok := r.Body.(interface{ ValidatedValue() any }); ok {
				value := b.ValidatedValue()
				if vr.body2020 != nil {
					if err := vr.body2020.validate(r.Header.Get("Content-Type"), value); err != nil {
						WriteError(w, r, logger, err)
						return
					}
				}
				r.Body = validatedBody{value: value}
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// validatedBody is a request body that only offers its validated value.
type validatedBody struct{ value any }

func (b validatedBody) ValidatedValue() any    { return b.value }
func (validatedBody) Read([]byte) (int, error) { return 0, errValidatedBody }
func (validatedBody) Close() error             { return nil }

var _ io.ReadCloser = validatedBody{}

func bodyLimit(op *openapi3.Operation) (int64, error) {
	v, ok := op.Extensions[ExtBodyLimit]
	if !ok {
		return 0, nil
	}
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	default:
		return 0, fmt.Errorf("%s must be a positive integer, got %T", ExtBodyLimit, v)
	}
	if n < 1 || n != math.Trunc(n) || n > math.MaxInt32 {
		return 0, fmt.Errorf("%s must be a positive integer of at most %d bytes", ExtBodyLimit, math.MaxInt32)
	}
	return int64(n), nil
}

// schemaUses2020Here reports whether s itself, not its children, uses a
// 2020-only keyword.
func schemaUses2020Here(s *openapi3.Schema) bool {
	return s.Const != nil || len(s.PrefixItems) > 0 || s.Contains != nil || s.MinContains != nil || s.MaxContains != nil ||
		len(s.PatternProperties) > 0 || len(s.DependentSchemas) > 0 || s.PropertyNames != nil ||
		s.UnevaluatedItems.Has != nil || s.UnevaluatedItems.Schema != nil ||
		s.UnevaluatedProperties.Has != nil || s.UnevaluatedProperties.Schema != nil ||
		s.If != nil || s.Then != nil || s.Else != nil || len(s.DependentRequired) > 0 ||
		len(s.Defs) > 0 || s.ContentSchema != nil || s.DynamicRef != "" || s.Nullable
}

// outcome is what one validator error means for the response.
type outcome uint8

const (
	outcomeFields outcome = iota
	outcomeMalformed
	outcomeMediaType
	outcomeUnknown
)

// requestValidationError turns a kin-openapi request validation error into
// the error WriteError renders. Client errors carry no cause: the
// validator's messages may contain request values. An error it cannot
// classify becomes a server error described by types only.
func requestValidationError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return tooLarge // WriteError makes it 413 REQUEST_BODY_TOO_LARGE
	}
	var fields []apperr.FieldError
	seen := map[outcome]bool{}
	for _, re := range requestErrors(err, nil) {
		f, o := classify(re)
		fields = append(fields, f...)
		seen[o] = true
	}
	switch {
	case seen[outcomeUnknown]:
		return fmt.Errorf("validate request: unclassified validator error: %s", describe(err))
	case seen[outcomeMediaType]:
		return apperr.New(apperr.KindUnsupportedMediaType, CodeContentTypeUnsupported)
	case seen[outcomeMalformed] && len(fields) == 0:
		return apperr.New(apperr.KindInvalid, CodeBodyMalformed)
	}
	return apperr.New(apperr.KindInvalid, CodeRequestInvalid).WithFields(sortFields(fields)...)
}

// sortFields orders field errors (body members first, then parameters)
// and drops duplicates, so a response does not depend on map order.
func sortFields(fields []apperr.FieldError) []apperr.FieldError {
	slices.SortFunc(fields, func(a, b apperr.FieldError) int {
		return strings.Compare(a.In+"\x00"+a.Pointer+a.Parameter+"\x00"+string(a.Code),
			b.In+"\x00"+b.Pointer+b.Parameter+"\x00"+string(b.Code))
	})
	return slices.Compact(fields)
}

// classify maps one validator error to field errors, or to an outcome
// without fields.
func classify(re *openapi3filter.RequestError) ([]apperr.FieldError, outcome) {
	var parseErr *openapi3filter.ParseError
	switch {
	case re.Parameter != nil:
		param := func(code apperr.Code) apperr.FieldError {
			return apperr.FieldError{Parameter: re.Parameter.Name, In: strings.ToUpper(re.Parameter.In), Code: code}
		}
		switch {
		case errors.Is(re.Err, openapi3filter.ErrInvalidRequired):
			return []apperr.FieldError{param(CodeFieldRequired)}, outcomeFields
		case errors.Is(re.Err, openapi3filter.ErrInvalidEmptyValue):
			return []apperr.FieldError{param(CodeFieldValueInvalid)}, outcomeFields
		case errors.As(re.Err, &parseErr) && parseErr.Kind == openapi3filter.KindInvalidFormat:
			// limit=abc: the same code as a mistyped body member.
			return []apperr.FieldError{param(CodeFieldTypeInvalid)}, outcomeFields
		}
		leaves := schemaLeaves(re.Err, nil)
		if len(leaves) == 0 {
			return nil, outcomeUnknown
		}
		out := make([]apperr.FieldError, 0, len(leaves))
		for _, se := range leaves {
			out = append(out, param(keywordCode(se.SchemaField)))
		}
		return out, outcomeFields

	case re.RequestBody != nil:
		switch {
		case errors.Is(re.Err, openapi3filter.ErrInvalidRequired):
			return nil, outcomeMalformed // no body where one is required
		case re.Err == nil && strings.HasPrefix(re.Reason, "header Content-Type has unexpected value"):
			return nil, outcomeMediaType
		case errors.As(re.Err, &parseErr):
			if parseErr.Kind == openapi3filter.KindInvalidFormat {
				return nil, outcomeMalformed
			}
			return nil, outcomeUnknown
		}
		leaves := schemaLeaves(re.Err, nil)
		if len(leaves) == 0 {
			return nil, outcomeUnknown
		}
		return bodyFields(leaves), outcomeFields

	case re.Err != nil && re.Err.Error() == "request body not allowed for this request":
		// RejectWhenRequestBodyNotSpecified: a body on an operation without one.
		return []apperr.FieldError{{Pointer: "", Code: CodeFieldUnknown}}, outcomeFields
	}
	return nil, outcomeUnknown
}

// describe names the validator errors by type and keyword only, for the
// server log: their messages may contain request values.
func describe(err error) string {
	var parts []string
	for _, re := range requestErrors(err, nil) {
		where := "request"
		switch {
		case re.Parameter != nil:
			where = re.Parameter.In + " parameter " + re.Parameter.Name
		case re.RequestBody != nil:
			where = "body"
		}
		part := fmt.Sprintf("%s: %T", where, re.Err)
		for _, se := range schemaLeaves(re.Err, nil) {
			part += " " + se.SchemaField
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
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

func bodyFields(leaves []*openapi3.SchemaError) []apperr.FieldError {
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
	case "type", "nullable":
		// "nullable": the OpenAPI 3.0 validator's report of a null where
		// the schema allows none, e.g. a merge patch clearing a member that
		// cannot be cleared.
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

// memberError returns the field code of se and, when se is about an
// unknown member rather than the object itself, that member's name: the
// OpenAPI 3.0 validator puts a missing member into the path, but names an
// unknown one only in its message (`property "x" is unsupported`). The
// tests pin this format, so a kin-openapi upgrade that changes it fails
// loudly.
func memberError(se *openapi3.SchemaError) (apperr.Code, []string) {
	if se.SchemaField == "properties" {
		if m := unsupportedProp.FindStringSubmatch(se.Reason); m != nil {
			if name, err := strconv.Unquote(m[1]); err == nil {
				return CodeFieldUnknown, []string{name}
			}
		}
	}
	return keywordCode(se.SchemaField), nil
}

var unsupportedProp = regexp.MustCompile(`property ("(?:[^"\\]|\\.)*") is unsupported`)

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
