package app

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/log"
)

func newAPIRouter(tb testing.TB, env config.Env) http.Handler {
	tb.Helper()
	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	router := httpx.NewRouter(logger, httpx.NewHealth(logger))
	if err := mountAPI(context.Background(), router, logger, env); err != nil {
		tb.Fatal(err)
	}
	return router
}

func ping(h http.Handler, contentType, body string) *httptest.ResponseRecorder {
	return callPing(h, http.MethodPost, contentType, body)
}

func callPing(h http.Handler, method, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, "/v1/meta/ping", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(httpx.HeaderRequestID, "req-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// POST /v1/meta/ping runs the whole contract path: the OpenAPI 3.1 spec is
// loaded, the request is validated against it, the generated strict server
// binds the validated value and the response is the generated type
// (ADR-0019). After validation the raw body cannot be read (httpx
// TestValidateRequestsHandsOnlyTheValidatedValue), so every 200 here was
// bound from the validated value, not from a second decode.
func TestPing(t *testing.T) {
	t.Parallel()

	h := newAPIRouter(t, config.EnvTest)
	const problem = `{"type":"https://api.iskoala.com/problems/request-invalid","title":"Bad Request",` +
		`"status":400,"code":"REQUEST_INVALID","instance":"/v1/meta/ping","requestId":"req-1","errors":`
	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantBody    string
	}{
		{
			name:       "null in a type array",
			body:       `{"amount":{"amount":15000,"currency":"TRY"},"note":null}`,
			wantStatus: http.StatusOK,
			wantBody:   `{"amount":{"amount":15000,"currency":"TRY"},"note":null}`,
		},
		{
			name:       "absent nullable is null in the response; largest safe amount",
			body:       `{"amount":{"amount":9007199254740991,"currency":"TRY"}}`,
			wantStatus: http.StatusOK,
			wantBody:   `{"amount":{"amount":9007199254740991,"currency":"TRY"},"note":null}`,
		},
		{
			name:       "string note, smallest safe amount",
			body:       `{"amount":{"amount":-9007199254740991,"currency":"TRY"},"note":"merhaba"}`,
			wantStatus: http.StatusOK,
			wantBody:   `{"amount":{"amount":-9007199254740991,"currency":"TRY"},"note":"merhaba"}`,
		},
		{
			name:       "missing member of a referenced schema",
			body:       `{"amount":{"amount":15000}}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   problem + `[{"pointer":"/amount/currency","code":"FIELD_REQUIRED"}]}`,
		},
		{
			name:       "float amount and unknown currency",
			body:       `{"amount":{"amount":150.5,"currency":"USD"}}`,
			wantStatus: http.StatusBadRequest,
			wantBody: problem + `[{"pointer":"/amount/amount","code":"FIELD_TYPE_INVALID"},` +
				`{"pointer":"/amount/currency","code":"FIELD_VALUE_INVALID"}]}`,
		},
		{
			name:       "unknown members, also in the nested money object",
			body:       `{"amount":{"amount":1,"currency":"TRY","precision":2},"Note":"x"}`,
			wantStatus: http.StatusBadRequest,
			wantBody: problem + `[{"pointer":"/Note","code":"FIELD_UNKNOWN"},` +
				`{"pointer":"/amount/precision","code":"FIELD_UNKNOWN"}]}`,
		},
		{
			name:       "note too long",
			body:       `{"amount":{"amount":1,"currency":"TRY"},"note":"` + strings.Repeat("x", 201) + `"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   problem + `[{"pointer":"/note","code":"FIELD_OUT_OF_RANGE"}]}`,
		},
		{
			// Above 2^53-1 a JavaScript client cannot read the amount exactly.
			name:       "amount over the safe integer range",
			body:       `{"amount":{"amount":9007199254740992,"currency":"TRY"}}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   problem + `[{"pointer":"/amount/amount","code":"FIELD_OUT_OF_RANGE"}]}`,
		},
		{
			name:        "unsupported content type",
			contentType: "text/plain",
			body:        `{"amount":{"amount":1,"currency":"TRY"}}`,
			wantStatus:  http.StatusUnsupportedMediaType,
			wantBody: `{"type":"https://api.iskoala.com/problems/content-type-unsupported",` +
				`"title":"Unsupported Media Type","status":415,"code":"CONTENT_TYPE_UNSUPPORTED",` +
				`"instance":"/v1/meta/ping","requestId":"req-1"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ct := tt.contentType
			if ct == "" {
				ct = "application/json"
			}
			rec := ping(h, ct, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := strings.TrimSuffix(rec.Body.String(), "\n"); got != tt.wantBody {
				t.Fatalf("body mismatch\n got: %s\nwant: %s", got, tt.wantBody)
			}
			wantCT := "application/json"
			if tt.wantStatus != http.StatusOK {
				wantCT = httpx.ContentTypeProblem
			}
			if ct := rec.Header().Get("Content-Type"); ct != wantCT {
				t.Fatalf("Content-Type = %q, want %q", ct, wantCT)
			}
		})
	}
}

func TestPingWrongMethodIsProblem(t *testing.T) {
	t.Parallel()

	h := newAPIRouter(t, config.EnvTest)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/meta/ping", http.NoBody))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST, PATCH" {
		t.Fatalf("status %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

// In production the smoke test is not mounted: it answers exactly like a
// path that never existed, for every method.
func TestPingIsNotMountedInProduction(t *testing.T) {
	t.Parallel()

	h := newAPIRouter(t, config.EnvProduction)
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodGet} {
		req := httptest.NewRequestWithContext(context.Background(), method, "/v1/meta/ping",
			strings.NewReader(`{"amount":{"amount":1,"currency":"TRY"}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(httpx.HeaderRequestID, "req-1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		const want = `{"type":"https://api.iskoala.com/problems/route-not-found","title":"Not Found",` +
			`"status":404,"code":"ROUTE_NOT_FOUND","requestId":"req-1"}`
		if rec.Code != http.StatusNotFound || rec.Body.String() != want {
			t.Fatalf("%s: status %d, body %s", method, rec.Code, rec.Body)
		}
	}
}

// PATCH /v1/meta/ping takes a JSON Merge Patch (RFC 7396): an absent
// member is unchanged, null clears it, a value sets it. The three reach
// the handler as distinct states (nullable.Nullable), through validation
// and the generated strict server (stil rehberi §3).
func TestPatchPingMergePatch(t *testing.T) {
	t.Parallel()

	h := newAPIRouter(t, config.EnvTest)
	const mergePatch = "application/merge-patch+json"
	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantBody    string
	}{
		{"nothing sent, nothing changes", mergePatch, `{}`, http.StatusOK, `{"cleared":[],"set":[]}`},
		{"null clears", mergePatch, `{"note":null}`, http.StatusOK, `{"cleared":["note"],"set":[]}`},
		{
			"values set", mergePatch, `{"amount":{"amount":1,"currency":"TRY"},"note":"merhaba"}`,
			http.StatusOK, `{"cleared":[],"set":["amount","note"]}`,
		},
		{
			"a member that cannot be cleared", mergePatch, `{"amount":null}`, http.StatusBadRequest,
			`{"type":"https://api.iskoala.com/problems/request-invalid","title":"Bad Request","status":400,` +
				`"code":"REQUEST_INVALID","instance":"/v1/meta/ping","requestId":"req-1",` +
				`"errors":[{"pointer":"/amount","code":"FIELD_TYPE_INVALID"}]}`,
		},
		{
			"unknown member", mergePatch, `{"title":"x"}`, http.StatusBadRequest,
			`{"type":"https://api.iskoala.com/problems/request-invalid","title":"Bad Request","status":400,` +
				`"code":"REQUEST_INVALID","instance":"/v1/meta/ping","requestId":"req-1",` +
				`"errors":[{"pointer":"/title","code":"FIELD_UNKNOWN"}]}`,
		},
		{
			"plain JSON is not a merge patch", "application/json", `{"note":null}`, http.StatusUnsupportedMediaType,
			`{"type":"https://api.iskoala.com/problems/content-type-unsupported","title":"Unsupported Media Type",` +
				`"status":415,"code":"CONTENT_TYPE_UNSUPPORTED","instance":"/v1/meta/ping","requestId":"req-1"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := callPing(h, http.MethodPatch, tt.contentType, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := strings.TrimSuffix(rec.Body.String(), "\n"); got != tt.wantBody {
				t.Fatalf("body mismatch\n got: %s\nwant: %s", got, tt.wantBody)
			}
		})
	}
}

// Every code the HTTP platform returns is in the contract's ErrorCode list,
// from which the clients' code lists are generated (ADR-0007 madde 5).
func TestPlatformCodesAreInTheContract(t *testing.T) {
	t.Parallel()

	for _, c := range httpx.PlatformCodes() {
		if !apiv1.ErrorCode(c).Valid() {
			t.Errorf("code %s is not in api/components/problem.yaml ErrorCode", c)
		}
	}
}

// BenchmarkPing measures one validated request through the whole stack:
// router middleware, validation, binding and the response.
func BenchmarkPing(b *testing.B) {
	h := newAPIRouter(b, config.EnvTest)
	const body = `{"amount":{"amount":15000,"currency":"TRY"},"note":"merhaba"}`
	b.ReportAllocs()
	for b.Loop() {
		if rec := ping(h, "application/json", body); rec.Code != http.StatusOK {
			b.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	}
}
