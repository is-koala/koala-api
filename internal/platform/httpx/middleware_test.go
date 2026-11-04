package httpx_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

func newTestRouter(t *testing.T) (http.Handler, *logSink) {
	t.Helper()
	logger, sink := newLogger()
	r := httpx.NewRouter(logger, httpx.NewHealth(logger))
	r.Get("/v1/jobs/{id}", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Seen-Request-Id", httpx.RequestIDFrom(req.Context()))
		w.WriteHeader(http.StatusNoContent)
	})
	r.Get("/panic", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	r.Get("/panic-after-write", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("late kaboom")
	})
	r.Get("/abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	return r, sink
}

func do(h http.Handler, method, target string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, http.NoBody)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRequestIDIsGeneratedEchoedAndLogged(t *testing.T) {
	t.Parallel()

	h, sink := newTestRouter(t)
	rec := do(h, http.MethodGet, "/v1/jobs/42", nil)

	id := rec.Header().Get(httpx.HeaderRequestID)
	if len(id) != 32 {
		t.Fatalf("generated request id = %q, want 32 hex chars", id)
	}
	if seen := rec.Header().Get("X-Seen-Request-Id"); seen != id {
		t.Fatalf("handler saw %q, response has %q", seen, id)
	}
	if got := sink.find(t, "http request")["request_id"]; got != id {
		t.Fatalf("access log request_id = %v, want %q", got, id)
	}
}

func TestRequestIDAcceptsWellFormedIncomingID(t *testing.T) {
	t.Parallel()

	h, _ := newTestRouter(t)
	rec := do(h, http.MethodGet, "/v1/jobs/42", http.Header{"X-Request-Id": {"mobile-1.2_abc"}})

	if got := rec.Header().Get(httpx.HeaderRequestID); got != "mobile-1.2_abc" {
		t.Fatalf("request id = %q", got)
	}
}

func TestRequestIDReplacesMalformedIncomingID(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"has space",
		`{"msg":"forged"}`,
		strings.Repeat("a", 65),
	} {
		h, _ := newTestRouter(t)
		rec := do(h, http.MethodGet, "/v1/jobs/42", http.Header{"X-Request-Id": {bad}})
		if got := rec.Header().Get(httpx.HeaderRequestID); got == bad || len(got) != 32 {
			t.Errorf("incoming %q: request id = %q, want a generated one", bad, got)
		}
	}
}

func TestAccessLogRecordsRoutePatternNotQuery(t *testing.T) {
	t.Parallel()

	h, sink := newTestRouter(t)
	do(h, http.MethodGet, "/v1/jobs/42?phone=05551112233", nil)

	rec := sink.find(t, "http request")
	want := map[string]any{
		"method": "GET",
		"route":  "/v1/jobs/{id}",
		"path":   "/v1/jobs/42",
		"status": float64(http.StatusNoContent),
	}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("access log %s = %v, want %v", k, rec[k], v)
		}
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Error("access log lacks duration_ms")
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(line), "0555") {
		t.Fatalf("query string logged: %s", line)
	}
}

func TestRecoverWritesInternalProblemAndLogsStack(t *testing.T) {
	t.Parallel()

	h, sink := newTestRouter(t)
	rec := do(h, http.MethodGet, "/panic", nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	var p httpx.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a problem: %v", err)
	}
	if p.Code != apperr.CodeInternal || p.RequestID == "" {
		t.Fatalf("problem = %+v", p)
	}
	if strings.Contains(rec.Body.String(), "kaboom") {
		t.Fatal("panic value leaked to client")
	}

	logged := sink.find(t, "panic")
	if logged["panic"] != "kaboom" || logged["request_id"] != p.RequestID {
		t.Fatalf("panic log = %v", logged)
	}
	if stack, _ := logged["stack"].(string); !strings.Contains(stack, "middleware_test.go") {
		t.Fatal("stack does not point at the panicking handler")
	}
	if got := sink.find(t, "http request")["status"]; got != float64(http.StatusInternalServerError) {
		t.Fatalf("access log status = %v, want 500", got)
	}
}

func TestRecoverDoesNotWriteTwice(t *testing.T) {
	t.Parallel()

	h, _ := newTestRouter(t)
	rec := do(h, http.MethodGet, "/panic-after-write", nil)

	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("got %d %q, want the handler's 202 untouched", rec.Code, rec.Body.String())
	}
}

func TestRecoverReraisesAbortHandler(t *testing.T) {
	t.Parallel()

	h, _ := newTestRouter(t)
	defer func() {
		if v := recover(); v != http.ErrAbortHandler { //nolint:errorlint // identity check on the recovered value
			t.Fatalf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	do(h, http.MethodGet, "/abort", nil)
}

func TestUnknownRouteIsProblem(t *testing.T) {
	t.Parallel()

	h, _ := newTestRouter(t)
	rec := do(h, http.MethodGet, "/nope", nil)

	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != httpx.ContentTypeProblem {
		t.Fatalf("got %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), `"code":"ROUTE_NOT_FOUND"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestWrongMethodIsProblemWithAllow(t *testing.T) {
	t.Parallel()

	h, _ := newTestRouter(t)
	rec := do(h, http.MethodDelete, "/v1/jobs/42", nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow = %q, want GET", allow)
	}
	if !strings.Contains(rec.Body.String(), `"code":"METHOD_NOT_ALLOWED"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
