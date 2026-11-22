package httpx_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"

	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// validateSpec has three operations:
//   - POST /things/{id} uses no JSON Schema 2020-only keyword, so it is
//     validated by kin-openapi's OpenAPI 3.0 validator (no per-request
//     compile; see ValidateRequests).
//   - POST /strict uses const and unevaluatedProperties, so its body is
//     also checked against the schema compiled once at start-up with the
//     2020-12 validator. Both must report the same field errors.
//   - POST /big raises its body limit with x-body-limit.
//   - PUT /gallery carries the "at most one cover" invariant of the real
//     contract's ReplaceCompanyGalleryRequest, keyword for keyword
//     (ADR-0031 Karar 1): contains + minContains: 0 + maxContains: 1.
//     The real operation is behind authn, so this is where the rule's
//     rejection is observed end to end until its handler exists.
const validateSpec = `
openapi: 3.1.1
info: {title: validate test, version: "1"}
paths:
  /things/{id}:
    post:
      operationId: postThing
      parameters:
        - {name: id, in: path, required: true, schema: {type: string, format: uuid}}
        - {name: limit, in: query, schema: {type: integer, format: int32, minimum: 1, maximum: 100}}
        - {name: X-Key, in: header, required: true, schema: {type: string}}
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              additionalProperties: false
              required: [name, count]
              properties:
                name: {type: string, maxLength: 5}
                count: {type: [integer, "null"], format: int32}
                kind: {type: string, enum: [THING]}
                at: {type: string, format: date-time}
                tag: {type: string, default: none}
      responses:
        "200": {description: ok}
  /strict:
    post:
      operationId: postStrict
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              allOf:
                - {type: object, properties: {name: {$ref: "#/components/schemas/Name"}}}
              unevaluatedProperties: false
              required: [name, kind]
              properties:
                kind: {type: string, const: THING}
      responses:
        "204": {description: ok}
  /big:
    post:
      operationId: postBig
      x-body-limit: 3145728
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              additionalProperties: false
              properties:
                blob: {type: string}
      responses:
        "204": {description: ok}
  /plain:
    get:
      operationId: getPlain
      responses:
        "204": {description: ok}
  /gallery:
    put:
      operationId: putGallery
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              additionalProperties: false
              required: [items]
              properties:
                items:
                  type: array
                  maxItems: 10
                  items:
                    type: object
                    additionalProperties: false
                    required: [mediaId]
                    properties:
                      mediaId: {type: string}
                      cover: {type: boolean}
                  contains:
                    type: object
                    required: [cover]
                    properties:
                      cover: {const: true}
                  minContains: 0
                  maxContains: 1
      responses:
        "204": {description: ok}
components:
  schemas:
    Name: {type: string, maxLength: 5}
`

const validID = "0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a10"

func loadValidateSpec(t *testing.T, spec string) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return doc
}

// echoValidated answers with what the handler can get from the body: the
// validated value as JSON, and whether reading the raw body failed.
func echoValidated(w http.ResponseWriter, r *http.Request) {
	_, readErr := io.ReadAll(r.Body)
	v, ok := r.Body.(interface{ ValidatedValue() any })
	if !ok {
		http.Error(w, "no validated value", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Read-Failed", strconv.FormatBool(readErr != nil))
	if err := json.NewEncoder(w).Encode(v.ValidatedValue()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func newValidateRouter(t *testing.T) http.Handler {
	t.Helper()
	logger, _ := newLogger()
	validate, err := httpx.ValidateRequests(loadValidateSpec(t, validateSpec), logger)
	if err != nil {
		t.Fatal(err)
	}
	r := newRouter(t, logger, httpx.NewHealth(logger))
	r.Group(func(g chi.Router) {
		g.Use(validate)
		noContent := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
		g.Post("/things/{id}", echoValidated)
		g.Post("/strict", noContent)
		g.Post("/big", noContent)
		g.Get("/plain", noContent)
		g.Put("/gallery", noContent)
		g.Get("/not-in-spec", noContent)
	})
	return withClient(r)
}

type problemBody struct {
	Code     string           `json:"code"`
	Instance string           `json:"instance"`
	Errors   []map[string]any `json:"errors"`
}

func bodyErr(pointer, code string) map[string]any {
	return map[string]any{"pointer": pointer, "code": code}
}

func paramErr(name, in, code string) map[string]any {
	return map[string]any{"parameter": name, "in": in, "code": code}
}

type validateCase struct {
	name       string
	method     string // POST if empty
	target     string
	header     map[string]string // overrides; "" removes the header
	body       string
	wantStatus int
	wantCode   string
	wantErrors []map[string]any
}

func runValidateCases(t *testing.T, h http.Handler, tests []validateCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			method := tt.method
			if method == "" {
				method = http.MethodPost
			}
			req := httptest.NewRequestWithContext(context.Background(), method, tt.target, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Key", "k")
			for k, v := range tt.header {
				if v == "" {
					req.Header.Del(k)
				} else {
					req.Header.Set(k, v)
				}
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantCode == "" {
				return
			}
			var got problemBody
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body is not a problem: %v: %s", err, rec.Body)
			}
			if got.Code != tt.wantCode || !reflect.DeepEqual(got.Errors, tt.wantErrors) {
				t.Fatalf("got %s %v, want %s %v", got.Code, got.Errors, tt.wantCode, tt.wantErrors)
			}
		})
	}
}

func TestValidateRequests(t *testing.T) {
	t.Parallel()

	thing := "/things/" + validID
	runValidateCases(t, newValidateRouter(t), []validateCase{
		{
			name:       "valid, null in a type array",
			target:     thing + "?limit=10",
			body:       `{"name":"a","count":null,"kind":"THING","at":"2026-08-27T09:30:00+03:00"}`,
			wantStatus: http.StatusOK,
		},
		{
			name: "missing members are named", target: thing, body: `{}`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID",
			wantErrors: []map[string]any{bodyErr("/count", "FIELD_REQUIRED"), bodyErr("/name", "FIELD_REQUIRED")},
		},
		{
			name: "unknown members are named and escaped", target: thing,
			body:       `{"name":"a","count":1,"Name":"b","a/b'c":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID",
			wantErrors: []map[string]any{bodyErr("/Name", "FIELD_UNKNOWN"), bodyErr("/a~1b'c", "FIELD_UNKNOWN")},
		},
		{
			name: "type, length, enum and format", target: thing,
			body:       `{"name":"toolong","count":"1","kind":"OTHER","at":"2026-13-01"}`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID",
			wantErrors: []map[string]any{
				bodyErr("/at", "FIELD_FORMAT_INVALID"), bodyErr("/count", "FIELD_TYPE_INVALID"),
				bodyErr("/kind", "FIELD_VALUE_INVALID"), bodyErr("/name", "FIELD_OUT_OF_RANGE"),
			},
		},
		{
			name: "whole body of the wrong type", target: thing, body: `[]`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID",
			wantErrors: []map[string]any{bodyErr("", "FIELD_TYPE_INVALID")},
		},
		{
			name: "parameters", target: "/things/not-a-uuid?limit=500",
			header: map[string]string{"X-Key": ""}, body: `{"name":"a","count":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID",
			wantErrors: []map[string]any{
				paramErr("X-Key", "HEADER", "FIELD_REQUIRED"),
				paramErr("id", "PATH", "FIELD_FORMAT_INVALID"),
				paramErr("limit", "QUERY", "FIELD_OUT_OF_RANGE"),
			},
		},
		{
			// The same code as a mistyped body member.
			name: "parameter of the wrong type", target: thing + "?limit=abc", body: `{"name":"a","count":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID",
			wantErrors: []map[string]any{paramErr("limit", "QUERY", "FIELD_TYPE_INVALID")},
		},
		{
			name: "empty body", target: thing,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_BODY_MALFORMED",
		},
		{
			name: "malformed JSON", target: thing, body: `{"name":`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_BODY_MALFORMED",
		},
		{
			name: "unsupported content type", target: thing,
			header: map[string]string{"Content-Type": "text/plain"}, body: `{"name":"a","count":1}`,
			wantStatus: http.StatusUnsupportedMediaType, wantCode: "CONTENT_TYPE_UNSUPPORTED",
		},
		{
			name: "body over the default limit", target: thing,
			body:       `{"name":"a","count":1}` + strings.Repeat(" ", int(httpx.DefaultBodyLimit)),
			wantStatus: http.StatusRequestEntityTooLarge, wantCode: "REQUEST_BODY_TOO_LARGE",
		},
		{
			name: "x-body-limit raises the limit", target: "/big",
			body:       `{"blob":"` + strings.Repeat("x", 2*int(httpx.DefaultBodyLimit)) + `"}`,
			wantStatus: http.StatusNoContent,
		},
		{
			name: "x-body-limit still limits", target: "/big",
			body:       `{"blob":"` + strings.Repeat("x", 3*int(httpx.DefaultBodyLimit)) + `"}`,
			wantStatus: http.StatusRequestEntityTooLarge, wantCode: "REQUEST_BODY_TOO_LARGE",
		},
		{
			name: "body on an operation without one", method: http.MethodGet, target: "/plain", body: `{}`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID",
			wantErrors: []map[string]any{bodyErr("", "FIELD_UNKNOWN")},
		},
	})
}

// The JSON Schema 2020-12 validator reports members at the object and
// names them in its message; the field errors are the same.
func TestValidateRequestsJSONSchema2020(t *testing.T) {
	t.Parallel()

	runValidateCases(t, newValidateRouter(t), []validateCase{
		{name: "valid", target: "/strict", body: `{"name":"a","kind":"THING"}`, wantStatus: http.StatusNoContent},
		{
			name: "missing members are named", target: "/strict", body: `{}`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID",
			wantErrors: []map[string]any{bodyErr("/kind", "FIELD_REQUIRED"), bodyErr("/name", "FIELD_REQUIRED")},
		},
		{
			name: "unevaluated members are unknown", target: "/strict", body: `{"name":"a","kind":"THING","x":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID",
			wantErrors: []map[string]any{bodyErr("/x", "FIELD_UNKNOWN")},
		},
		{
			// The media type is matched as kin-openapi matches it.
			name: "content type with parameters and case", target: "/strict",
			header: map[string]string{"Content-Type": "Application/JSON; charset=utf-8"},
			body:   `{"name":"a","kind":"THING"}`, wantStatus: http.StatusNoContent,
		},
		{
			name: "const", target: "/strict", body: `{"name":"a","kind":"OTHER"}`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID",
			wantErrors: []map[string]any{bodyErr("/kind", "FIELD_VALUE_INVALID")},
		},
	})
}

// TestValidateRequestsAtMostOneCover is the "at most one cover"
// invariant of ADR-0031 Karar 1, observed where it is actually enforced:
// a real router, the real ValidateRequests, and the same three keywords
// the contract carries. Without it the rule would be written in the
// schema and asserted nowhere -- dropping maxContains, or moving
// fields2020's *kind.MaxContains branch into its default, would leave
// every job green.
//
// The three passing cases are the compatibility promise: a client that
// never sends the field, an empty gallery, and a client that sends the
// field as false everywhere are all accepted. They are what minContains:
// 0 buys, and TestFields2020 shows what the same schema does without it.
func TestValidateRequestsAtMostOneCover(t *testing.T) {
	t.Parallel()

	item := func(id string, cover string) string {
		if cover == "" {
			return `{"mediaId":"` + id + `"}`
		}
		return `{"mediaId":"` + id + `","cover":` + cover + `}`
	}
	many := func(n int, cover string) string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, item("m"+strconv.Itoa(i), cover))
		}
		return `{"items":[` + strings.Join(out, ",") + `]}`
	}
	outOfRange := []map[string]any{bodyErr("/items", "FIELD_OUT_OF_RANGE")}

	runValidateCases(t, newValidateRouter(t), []validateCase{
		{
			name: "one cover", method: http.MethodPut, target: "/gallery",
			body: `{"items":[` + item("a", "") + `,` + item("b", "true") + `]}`, wantStatus: http.StatusNoContent,
		},
		{
			name: "no cover marked: the old client's body", method: http.MethodPut, target: "/gallery",
			body: `{"items":[` + item("a", "") + `,` + item("b", "") + `]}`, wantStatus: http.StatusNoContent,
		},
		{
			name: "empty gallery", method: http.MethodPut, target: "/gallery",
			body: `{"items":[]}`, wantStatus: http.StatusNoContent,
		},
		{
			name: "every item cover false", method: http.MethodPut, target: "/gallery",
			body: many(3, "false"), wantStatus: http.StatusNoContent,
		},
		{
			name: "two covers", method: http.MethodPut, target: "/gallery",
			body:       `{"items":[` + item("a", "true") + `,` + item("b", "true") + `]}`,
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID", wantErrors: outOfRange,
		},
		{
			name: "every item cover true", method: http.MethodPut, target: "/gallery",
			body:       many(10, "true"),
			wantStatus: http.StatusBadRequest, wantCode: "REQUEST_INVALID", wantErrors: outOfRange,
		},
	})
}

// A parameter with a 2020-only keyword cannot be checked without kin's
// per-request compile, so it is refused at start-up.
func TestValidateRequestsRefuses2020ParameterSchemas(t *testing.T) {
	t.Parallel()

	spec := strings.Replace(validateSpec,
		"{name: limit, in: query, schema: {type: integer, format: int32, minimum: 1, maximum: 100}}",
		"{name: limit, in: query, schema: {type: integer, format: int32, const: 10}}", 1)
	logger, _ := newLogger()
	if _, err := httpx.ValidateRequests(loadValidateSpec(t, spec), logger); err == nil {
		t.Fatal("a parameter schema with const was accepted")
	}
}

// A 2020-only keyword under not or oneOf would make the 3.0 validator,
// which ignores it, reject valid bodies; such a schema is refused.
//
// The last two shapes are the ones that used to escape. The gate walked
// seven of the nineteen slots a schema can nest another schema in, so a
// `not` or a `oneOf` sitting under one of the other twelve was never
// reached: the spec was accepted, the 3.0 validator ran alone and
// ignored the keyword, and `not` then refused every body the operation
// was sent -- a silent outage behind four green jobs. One walk answers
// both questions now (ADR-0031 Karar 5).
func TestValidateRequestsRefuses2020UnderNotOrOneOf(t *testing.T) {
	t.Parallel()

	for name, schema := range map[string]string{
		"not":                "{type: object, not: {const: {}}}",
		"oneOf":              "{type: object, properties: {kind: {oneOf: [{const: A}, {const: B}]}}}",
		"not under contains": "{type: array, contains: {not: {const: 1}}}",
		"oneOf under then":   "{type: object, if: {const: 1}, then: {oneOf: [{const: A}, {const: B}]}}",
	} {
		spec := strings.Replace(validateSpec,
			"              type: object\n              additionalProperties: false\n              properties:\n                blob: {type: string}",
			"              "+schema, 1)
		if spec == validateSpec {
			t.Fatal("test spec did not change")
		}
		logger, _ := newLogger()
		if _, err := httpx.ValidateRequests(loadValidateSpec(t, spec), logger); err == nil {
			t.Errorf("%s: a 2020-only keyword under %s was accepted", name, name)
		}
	}
}

// BenchmarkValidateRequests measures one valid request through the router
// and the validator: on the 3.0 path, and on an operation whose body is
// also checked against its precompiled JSON Schema 2020 schema.
func BenchmarkValidateRequests(b *testing.B) {
	logger, _ := newLogger()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(validateSpec))
	if err != nil {
		b.Fatal(err)
	}
	validate, err := httpx.ValidateRequests(doc, logger)
	if err != nil {
		b.Fatal(err)
	}
	r := newRouter(b, logger, httpx.NewHealth(logger))
	h := withClient(r)
	r.Group(func(g chi.Router) {
		g.Use(validate)
		noContent := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
		g.Post("/things/{id}", noContent)
		g.Post("/strict", noContent)
	})
	for _, bc := range []struct{ name, target, body string }{
		{"openapi30", "/things/" + validID + "?limit=10", `{"name":"a","count":1,"kind":"THING"}`},
		{"jsonschema2020", "/strict", `{"name":"a","kind":"THING"}`},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, bc.target, strings.NewReader(bc.body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Key", "k")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusNoContent {
					b.Fatalf("status %d: %s", rec.Code, rec.Body)
				}
			}
		})
	}
}

// After validation the handler gets the validated value, defaults filled
// in, and cannot read the raw body: nothing can decode it a second time
// with different rules (encoding/json, for one, matches member names
// case-insensitively).
func TestValidateRequestsHandsOnlyTheValidatedValue(t *testing.T) {
	t.Parallel()

	h := newValidateRouter(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/things/"+validID,
		strings.NewReader(`{"name":"a","count":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Read-Failed") != "true" {
		t.Fatal("the handler could read the raw body after validation")
	}
	if got, want := strings.TrimSpace(rec.Body.String()), `{"count":1,"name":"a","tag":"none"}`; got != want {
		t.Fatalf("validated value = %s, want %s", got, want)
	}
}

// Validator messages carry request values; none may reach the client, and
// the problem's instance is the route template, not the path.
func TestValidateRequestsNeverEchoesValues(t *testing.T) {
	t.Parallel()

	h := newValidateRouter(t)
	const secret = "TR330006100519786457841326"
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/things/"+secret, strings.NewReader(`{"name":"`+secret+`","count":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("problem echoes a request value: %s", rec.Body)
	}
	var got problemBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Instance != "/things/{id}" {
		t.Fatalf("instance = %q (%v), want the route template", got.Instance, err)
	}
}

// A route the spec does not describe is a wiring bug: it fails closed.
func TestValidateRequestsRejectsRoutesWithoutOperation(t *testing.T) {
	t.Parallel()

	h := newValidateRouter(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/not-in-spec", http.NoBody))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestValidateRequestsRejectsBadBodyLimit(t *testing.T) {
	t.Parallel()

	for _, limit := range []string{`"big"`, "0", "1.5"} {
		spec := strings.Replace(validateSpec, "x-body-limit: 3145728", "x-body-limit: "+limit, 1)
		logger, _ := newLogger()
		if _, err := httpx.ValidateRequests(loadValidateSpec(t, spec), logger); err == nil {
			t.Errorf("x-body-limit: %s accepted", limit)
		}
	}
}
