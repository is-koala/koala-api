package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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
	r.Get("/deadline", func(w http.ResponseWriter, req *http.Request) {
		if dl, ok := req.Context().Deadline(); ok {
			w.Header().Set("X-Deadline-In-Ms", strconv.FormatInt(time.Until(dl).Milliseconds(), 10))
		}
		w.WriteHeader(http.StatusNoContent)
	})
	readAll := func(w http.ResponseWriter, req *http.Request) {
		if _, err := io.ReadAll(req.Body); err != nil {
			httpx.WriteError(w, req, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	r.Post("/echo", readAll)
	r.With(httpx.BodyLimit(2<<20)).Post("/upload", readAll)
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

// recovered runs f and returns the value it panicked with, or nil.
func recovered(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
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

func TestAccessLogRecordsRouteNotPathOrQuery(t *testing.T) {
	t.Parallel()

	h, sink := newTestRouter(t)
	do(h, http.MethodGet, "/v1/jobs/42?phone=05551112233", nil)

	rec := sink.find(t, "http request")
	want := map[string]any{
		"method": "GET",
		"route":  "/v1/jobs/{id}",
		"status": float64(http.StatusNoContent),
	}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("access log %s = %v, want %v", k, rec[k], v)
		}
	}
	if _, ok := rec["path"]; ok {
		t.Errorf("matched route logged its raw path: %v", rec)
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Error("access log lacks duration_ms")
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(line), "0555") || strings.Contains(string(line), "/v1/jobs/42") {
		t.Fatalf("path or query string logged: %s", line)
	}
}

func TestAccessLogRecordsPathOnlyForUnmatchedRoutes(t *testing.T) {
	t.Parallel()

	h, sink := newTestRouter(t)
	do(h, http.MethodGet, "/nope?token=x", nil)

	rec := sink.find(t, "http request")
	if rec["route"] != "" || rec["path"] != "/nope" || rec["status"] != float64(http.StatusNotFound) {
		t.Fatalf("access log = %v, want empty route, path /nope, status 404", rec)
	}
}

func TestAccessLogIsWrittenWhenTheResponseIsAborted(t *testing.T) {
	t.Parallel()

	h, sink := newTestRouter(t)
	_ = recovered(func() { do(h, http.MethodGet, "/abort", nil) })

	rec := sink.find(t, "http request")
	if rec["aborted"] != true || rec["route"] != "/abort" {
		t.Fatalf("access log = %v, want an aborted line for /abort", rec)
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

func TestRecoverAbortsWhenHeadersAreAlreadyWritten(t *testing.T) {
	t.Parallel()

	h, sink := newTestRouter(t)
	var rec *httptest.ResponseRecorder
	v := recovered(func() { rec = do(h, http.MethodGet, "/panic-after-write", nil) })

	if v != http.ErrAbortHandler { //nolint:errorlint // identity check on the recovered value
		t.Fatalf("recovered %v, want http.ErrAbortHandler", v)
	}
	if rec != nil {
		t.Fatal("do returned; the response must be aborted")
	}
	if logged := sink.find(t, "panic"); logged["panic"] != "late kaboom" {
		t.Fatalf("panic log = %v", logged)
	}
	if line := sink.find(t, "http request"); line["status"] != float64(http.StatusAccepted) || line["aborted"] != true {
		t.Fatalf("access log = %v, want status 202 and aborted", line)
	}
}

func TestRecoverReraisesAbortHandler(t *testing.T) {
	t.Parallel()

	h, sink := newTestRouter(t)
	v := recovered(func() { do(h, http.MethodGet, "/abort", nil) })

	if v != http.ErrAbortHandler { //nolint:errorlint // identity check on the recovered value
		t.Fatalf("recovered %v, want http.ErrAbortHandler", v)
	}
	for _, r := range sink.records(t) {
		if r["msg"] == "panic" {
			t.Fatalf("deliberate abort logged as a panic: %v", r)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	h, _ := newTestRouter(t)
	for _, target := range []string{"/v1/jobs/42", "/nope", "/livez"} {
		rec := do(h, http.MethodGet, target, nil)
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", target, got)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
			t.Errorf("%s: Content-Security-Policy = %q", target, got)
		}
	}
}

func TestHealthEndpointsAcceptHead(t *testing.T) {
	t.Parallel()

	h, _ := newTestRouter(t)
	for _, target := range []string{httpx.PathLivez, httpx.PathReadyz} {
		if rec := do(h, http.MethodHead, target, nil); rec.Code != http.StatusOK {
			t.Errorf("HEAD %s = %d, want 200", target, rec.Code)
		}
		rec := do(h, http.MethodPost, target, nil)
		if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("POST %s: Allow = %q, want GET, HEAD", target, allow)
		}
	}
}

func TestRequestContextHasRequestTimeout(t *testing.T) {
	t.Parallel()

	h, _ := newTestRouter(t)
	rec := do(h, http.MethodGet, "/deadline", nil)

	ms, err := strconv.ParseInt(rec.Header().Get("X-Deadline-In-Ms"), 10, 64)
	if err != nil {
		t.Fatalf("request context has no deadline: %v", err)
	}
	remaining := time.Duration(ms) * time.Millisecond
	if remaining > httpx.RequestTimeout || remaining < httpx.RequestTimeout-5*time.Second {
		t.Fatalf("deadline in %s, want about %s", remaining, httpx.RequestTimeout)
	}
}

func TestTimeoutCancelsTheRequestContext(t *testing.T) {
	t.Parallel()

	got := make(chan error, 1)
	h := httpx.Timeout(20 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			got <- r.Context().Err()
		case <-time.After(5 * time.Second):
			got <- nil
		}
	}))
	do(h, http.MethodGet, "/", nil)

	if err := <-got; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handler saw %v, want context.DeadlineExceeded", err)
	}
}

func post(h http.Handler, target string, size int, chunked bool) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, bytes.NewReader(make([]byte, size)))
	if chunked {
		req.ContentLength = -1
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBodyLimit(t *testing.T) {
	t.Parallel()

	const limit = int(httpx.DefaultBodyLimit)
	tests := []struct {
		name    string
		target  string
		size    int
		chunked bool
		want    int
	}{
		{"default at limit", "/echo", limit, false, http.StatusNoContent},
		{"default over limit by Content-Length", "/echo", limit + 1, false, http.StatusRequestEntityTooLarge},
		{"default over limit while reading a chunked body", "/echo", limit + 1, true, http.StatusRequestEntityTooLarge},
		{"route raises the limit", "/upload", limit + limit/2, false, http.StatusNoContent},
		{"route raises the limit, chunked", "/upload", limit + limit/2, true, http.StatusNoContent},
		{"over the raised limit", "/upload", 2*limit + 1, true, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h, _ := newTestRouter(t)
			rec := post(h, tt.target, tt.size, tt.chunked)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want != http.StatusRequestEntityTooLarge {
				return
			}
			var p httpx.Problem
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatalf("body is not a problem: %v", err)
			}
			if p.Code != httpx.CodeBodyTooLarge || p.Status != http.StatusRequestEntityTooLarge {
				t.Fatalf("problem = %+v", p)
			}
		})
	}
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
