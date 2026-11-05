package httpx_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"

	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// validateSpec has no $ref, so kin-openapi validates it with its JSON
// Schema 2020-12 validator (the OpenAPI 3.1 path). The contract's own
// schemas, which use $ref, take kin-openapi's OpenAPI 3.0 validator; their
// error format is pinned by internal/app's ping tests.
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
                kind: {type: string, const: THING}
                at: {type: string, format: date-time}
      responses:
        "204": {description: ok}
`

const validID = "0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a10"

func newValidateRouter(t *testing.T) http.Handler {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(validateSpec))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	logger, _ := newLogger()
	validate, err := httpx.ValidateRequests(doc, logger)
	if err != nil {
		t.Fatal(err)
	}
	r := httpx.NewRouter(logger, httpx.NewHealth(logger))
	r.Group(func(g chi.Router) {
		g.Use(validate)
		ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
		g.Post("/things/{id}", ok)
		g.Get("/not-in-spec", ok)
	})
	return r
}

type problemBody struct {
	Code   string              `json:"code"`
	Errors []map[string]string `json:"errors"`
}

func TestValidateRequests(t *testing.T) {
	t.Parallel()

	h := newValidateRouter(t)
	tests := []struct {
		name        string
		target      string
		contentType string
		key         string
		body        string
		wantStatus  int
		wantCode    string
		wantErrors  []map[string]string
	}{
		{
			name:       "valid, null in a type array",
			target:     "/things/" + validID + "?limit=10",
			body:       `{"name":"a","count":null,"kind":"THING","at":"2026-08-27T09:30:00+03:00"}`,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "missing members are named",
			target:     "/things/" + validID,
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "REQUEST_INVALID",
			wantErrors: []map[string]string{
				{"pointer": "/count", "code": "FIELD_REQUIRED"},
				{"pointer": "/name", "code": "FIELD_REQUIRED"},
			},
		},
		{
			name:       "unknown members are named and escaped",
			target:     "/things/" + validID,
			body:       `{"name":"a","count":1,"Name":"b","a/b'c":1}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "REQUEST_INVALID",
			wantErrors: []map[string]string{
				{"pointer": "/Name", "code": "FIELD_UNKNOWN"},
				{"pointer": "/a~1b'c", "code": "FIELD_UNKNOWN"},
			},
		},
		{
			name:       "type, length, const and format",
			target:     "/things/" + validID,
			body:       `{"name":"toolong","count":"1","kind":"OTHER","at":"2026-13-01"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "REQUEST_INVALID",
			wantErrors: []map[string]string{
				{"pointer": "/at", "code": "FIELD_FORMAT_INVALID"},
				{"pointer": "/count", "code": "FIELD_TYPE_INVALID"},
				{"pointer": "/kind", "code": "FIELD_VALUE_INVALID"},
				{"pointer": "/name", "code": "FIELD_OUT_OF_RANGE"},
			},
		},
		{
			name:       "parameters",
			target:     "/things/not-a-uuid?limit=500",
			key:        "-",
			body:       `{"name":"a","count":1}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "REQUEST_INVALID",
			wantErrors: []map[string]string{
				{"pointer": "header:X-Key", "code": "FIELD_REQUIRED"},
				{"pointer": "path:id", "code": "FIELD_FORMAT_INVALID"},
				{"pointer": "query:limit", "code": "FIELD_OUT_OF_RANGE"},
			},
		},
		{
			name:       "empty body",
			target:     "/things/" + validID,
			wantStatus: http.StatusBadRequest,
			wantCode:   "REQUEST_INVALID",
			wantErrors: []map[string]string{{"pointer": "", "code": "FIELD_REQUIRED"}},
		},
		{
			name:        "wrong content type",
			target:      "/things/" + validID,
			contentType: "text/plain",
			body:        `{"name":"a","count":1}`,
			wantStatus:  http.StatusBadRequest,
			wantCode:    "REQUEST_INVALID",
			wantErrors:  []map[string]string{{"pointer": "header:Content-Type", "code": "FIELD_VALUE_INVALID"}},
		},
		{
			name:       "malformed JSON",
			target:     "/things/" + validID,
			body:       `{"name":`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "REQUEST_BODY_MALFORMED",
		},
		{
			name:       "body over the limit",
			target:     "/things/" + validID,
			body:       `{"name":"a","count":1}` + strings.Repeat(" ", int(httpx.DefaultBodyLimit)),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "REQUEST_BODY_TOO_LARGE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, tt.target, strings.NewReader(tt.body))
			ct := tt.contentType
			if ct == "" {
				ct = "application/json"
			}
			req.Header.Set("Content-Type", ct)
			if tt.key != "-" {
				req.Header.Set("X-Key", "k")
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

// Validator messages carry request values; none may reach the client.
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
	body := rec.Body.String()
	// The path is the problem's instance; strip it before looking.
	body = strings.ReplaceAll(body, `"instance":"/things/`+secret+`"`, "")
	if strings.Contains(body, secret) {
		t.Fatalf("problem echoes a request value: %s", body)
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
