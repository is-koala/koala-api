package httpx_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// serveError runs WriteError behind the RequestID middleware so the problem
// carries a request id, as in production.
func serveError(t *testing.T, err error) (*httptest.ResponseRecorder, *logSink) {
	t.Helper()
	logger, sink := newLogger()
	h := httpx.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, logger, err)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/job-assignments/abc/payment-session?token=secret", http.NoBody)
	req.Header.Set(httpx.HeaderRequestID, "req-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, sink
}

func TestWriteErrorGoldenBody(t *testing.T) {
	t.Parallel()

	err := apperr.New(apperr.KindConflict, "PAYMENT_ATTEMPT_ACTIVE").
		WithFields(apperr.FieldError{Pointer: "/iban", Code: "IBAN_INVALID"})
	rec, _ := serveError(t, err)

	const want = `{"type":"https://api.iskoala.com/problems/payment-attempt-active",` +
		`"title":"Conflict","status":409,"code":"PAYMENT_ATTEMPT_ACTIVE",` +
		`"instance":"/v1/job-assignments/abc/payment-session","requestId":"req-123",` +
		`"errors":[{"pointer":"/iban","code":"IBAN_INVALID"}]}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body mismatch\n got: %s\nwant: %s", got, want)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != httpx.ContentTypeProblem {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestStatusOfEveryKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind apperr.Kind
		want int
	}{
		{apperr.KindInvalid, http.StatusBadRequest},
		{apperr.KindUnauthenticated, http.StatusUnauthorized},
		{apperr.KindForbidden, http.StatusForbidden},
		{apperr.KindNotFound, http.StatusNotFound},
		{apperr.KindConflict, http.StatusConflict},
		{apperr.KindRateLimited, http.StatusTooManyRequests},
		{apperr.KindUpstream, http.StatusBadGateway},
		{apperr.KindPayloadTooLarge, http.StatusRequestEntityTooLarge},
		{apperr.KindUnavailable, http.StatusServiceUnavailable},
		{apperr.KindInternal, http.StatusInternalServerError},
		{apperr.Kind(200), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		if got := httpx.StatusOf(tt.kind); got != tt.want {
			t.Errorf("StatusOf(%s) = %d, want %d", tt.kind, got, tt.want)
		}
	}
}

func TestWriteErrorHidesInternalCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("pq: relation users does not exist")
	rec, sink := serveError(t, fmt.Errorf("load user: %w", cause))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "relation") {
		t.Fatalf("cause leaked to client: %s", body)
	}
	var p httpx.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Code != apperr.CodeInternal {
		t.Fatalf("code = %q, want INTERNAL", p.Code)
	}

	logged := sink.find(t, "request failed")
	if !strings.Contains(fmt.Sprint(logged["error"]), "relation users does not exist") {
		t.Fatalf("cause not logged: %v", logged)
	}
	if logged["request_id"] != "req-123" {
		t.Fatalf("request id not logged: %v", logged)
	}
}

func TestWriteErrorInternalKindNeverExposesItsCode(t *testing.T) {
	t.Parallel()

	rec, _ := serveError(t, apperr.New(apperr.KindInternal, "LEDGER_INVARIANT_BROKEN"))

	if strings.Contains(rec.Body.String(), "LEDGER") {
		t.Fatalf("internal code leaked: %s", rec.Body.String())
	}
}

func TestWriteErrorWithoutCodeIsInternal(t *testing.T) {
	t.Parallel()

	rec, _ := serveError(t, apperr.New(apperr.KindNotFound, ""))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for an error without code", rec.Code)
	}
}

func TestWriteErrorDoesNotLogClientErrors(t *testing.T) {
	t.Parallel()

	_, sink := serveError(t, apperr.New(apperr.KindNotFound, "JOB_NOT_FOUND"))

	for _, r := range sink.records(t) {
		if r["msg"] == "request failed" {
			t.Fatalf("4xx logged as failure: %v", r)
		}
	}
}

func TestWriteErrorMapsMaxBytesErrorTo413(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("decode body: %w", &http.MaxBytesError{Limit: 1 << 20})
	rec, sink := serveError(t, err)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"REQUEST_BODY_TOO_LARGE"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	for _, r := range sink.records(t) {
		if r["msg"] == "request failed" {
			t.Fatalf("413 logged as a server failure: %v", r)
		}
	}
}

// TestWriteErrorMapsWrappedMaxBytesErrorTo413 locks the 413 against a
// KindInvalid wrapper: a handler that wraps every decode failure as a 400
// must not turn an oversized body into a 400.
func TestWriteErrorMapsWrappedMaxBytesErrorTo413(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{"invalid wraps", apperr.Wrap(&http.MaxBytesError{Limit: 1 << 20}, apperr.KindInvalid, "VALIDATION_FAILED")},
		{"invalid wraps wrapped", fmt.Errorf("handler: %w",
			apperr.Wrap(fmt.Errorf("decode body: %w", &http.MaxBytesError{Limit: 1 << 20}), apperr.KindInvalid, "VALIDATION_FAILED"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec, _ := serveError(t, tt.err)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), `"code":"REQUEST_BODY_TOO_LARGE"`) {
				t.Fatalf("body = %s", rec.Body.String())
			}
		})
	}
}

// TestWriteErrorKeepsOtherKindsAroundMaxBytesError: only KindInvalid is
// overridden; a deliberate kind such as KindConflict stays.
func TestWriteErrorKeepsOtherKindsAroundMaxBytesError(t *testing.T) {
	t.Parallel()

	err := apperr.Wrap(&http.MaxBytesError{Limit: 1 << 20}, apperr.KindConflict, "JOB_ALREADY_TAKEN")
	rec, _ := serveError(t, err)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

// TestWriteErrorLogsUnavailableAsWarn: a 503 refusal (drain) is expected
// and must not page as an error.
func TestWriteErrorLogsUnavailableAsWarn(t *testing.T) {
	t.Parallel()

	rec, sink := serveError(t, apperr.New(apperr.KindUnavailable, "SERVICE_DRAINING"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := sink.find(t, "request failed")["level"]; got != "WARN" {
		t.Fatalf("level = %v, want WARN", got)
	}
}
