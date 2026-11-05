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
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/log"
)

func newAPIRouter(t *testing.T) http.Handler {
	t.Helper()
	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	router := httpx.NewRouter(logger, httpx.NewHealth(logger))
	if err := mountAPI(context.Background(), router, logger); err != nil {
		t.Fatal(err)
	}
	return router
}

func ping(h http.Handler, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/meta/ping", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(httpx.HeaderRequestID, "req-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// POST /v1/meta/ping runs the whole contract path: the OpenAPI 3.1 spec is
// loaded, the request is validated against it, the generated strict server
// binds it and the response is the generated type (ADR-0006 "Spike
// sonucu"). The ping schemas use $ref, so kin-openapi validates them with
// its OpenAPI 3.0 validator; this pins that validator's error format.
func TestPing(t *testing.T) {
	t.Parallel()

	h := newAPIRouter(t)
	const problem = `{"type":"https://api.iskoala.com/problems/request-invalid","title":"Bad Request",` +
		`"status":400,"code":"REQUEST_INVALID","instance":"/v1/meta/ping","requestId":"req-1","errors":`
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "null in a type array",
			body:       `{"amount":{"amount":15000,"currency":"TRY"},"note":null}`,
			wantStatus: http.StatusOK,
			wantBody:   `{"amount":{"amount":15000,"currency":"TRY"},"note":null}`,
		},
		{
			name:       "absent nullable is null in the response",
			body:       `{"amount":{"amount":9223372036854775807,"currency":"TRY"}}`,
			wantStatus: http.StatusOK,
			wantBody:   `{"amount":{"amount":9223372036854775807,"currency":"TRY"},"note":null}`,
		},
		{
			name:       "string note",
			body:       `{"amount":{"amount":0,"currency":"TRY"},"note":"merhaba"}`,
			wantStatus: http.StatusOK,
			wantBody:   `{"amount":{"amount":0,"currency":"TRY"},"note":"merhaba"}`,
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
			// encoding/json would bind "Note" to Note; the validator rejects it
			// and the handler binds the validated value, not a second decode.
			name:       "case variant of a member",
			body:       `{"amount":{"amount":1,"currency":"TRY"},"Note":"x"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   problem + `[{"pointer":"/Note","code":"FIELD_UNKNOWN"}]}`,
		},
		{
			name:       "note too long",
			body:       `{"amount":{"amount":1,"currency":"TRY"},"note":"` + strings.Repeat("x", 201) + `"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   problem + `[{"pointer":"/note","code":"FIELD_OUT_OF_RANGE"}]}`,
		},
		{
			// The int64 format check passes 2^63 (it compares as a float);
			// binding then fails and the body is reported as malformed.
			name:       "amount over int64",
			body:       `{"amount":{"amount":9223372036854775808,"currency":"TRY"}}`,
			wantStatus: http.StatusBadRequest,
			wantBody: `{"type":"https://api.iskoala.com/problems/request-body-malformed","title":"Bad Request",` +
				`"status":400,"code":"REQUEST_BODY_MALFORMED","instance":"/v1/meta/ping","requestId":"req-1"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := ping(h, "application/json", tt.body)
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

	h := newAPIRouter(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/meta/ping", http.NoBody))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("status %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
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
