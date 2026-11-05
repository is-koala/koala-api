package httpx_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// TestStreamRouteSkipsRequestBounds pins the split chain: a Stream route
// runs the root chain (request id, security headers) without the request
// Timeout, while routes mounted on the Router keep it.
func TestStreamRouteSkipsRequestBounds(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	r := httpx.NewRouter(logger, httpx.NewHealth(logger))
	reportDeadline := func(w http.ResponseWriter, req *http.Request) {
		has := "no"
		if _, ok := req.Context().Deadline(); ok {
			has = "yes"
		}
		w.Header().Set("X-Has-Deadline", has)
		w.WriteHeader(http.StatusNoContent)
	}
	r.Stream("/stream", reportDeadline)
	r.Get("/bounded", reportDeadline)
	r.Route("/v1", func(sub chi.Router) { sub.Get("/nested", reportDeadline) })

	tests := []struct {
		target string
		want   string
	}{
		{"/stream", "no"},
		{"/bounded", "yes"},
		{"/v1/nested", "yes"},
	}
	for _, tt := range tests {
		rec := do(r, http.MethodGet, tt.target, nil)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("GET %s = %d", tt.target, rec.Code)
		}
		if got := rec.Header().Get("X-Has-Deadline"); got != tt.want {
			t.Errorf("GET %s: deadline %s, want %s", tt.target, got, tt.want)
		}
		if rec.Header().Get(httpx.HeaderRequestID) == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: root chain headers missing: %v", tt.target, rec.Header())
		}
	}

	rec := do(r, http.MethodPost, "/stream", nil)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST /stream = %d, Allow %q; want 405, GET", rec.Code, rec.Header().Get("Allow"))
	}
}

// TestStreamRouteUnderMountedPrefix: a stream route inside a prefix that a
// module mounted keeps its own 405 instead of falling through to the
// sub-router's 404, and the sub-router still serves its own paths.
func TestStreamRouteUnderMountedPrefix(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	r := httpx.NewRouter(logger, httpx.NewHealth(logger))
	sub := chi.NewRouter()
	sub.Get("/jobs", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Mount("/v1", sub)
	r.Stream("/v1/realtime", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	tests := []struct {
		method string
		target string
		want   int
	}{
		{http.MethodGet, "/v1/realtime", http.StatusTeapot},
		{http.MethodPost, "/v1/realtime", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/v1/realtime", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/jobs", http.StatusNoContent},
	}
	for _, tt := range tests {
		if rec := do(r, tt.method, tt.target, nil); rec.Code != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.target, rec.Code, tt.want)
		}
	}
}

// fakeStream records the shutdown calls Serve makes.
type fakeStream struct {
	err       error
	drainOnce sync.Once
	drained   chan struct{}
	shutdown  chan struct{}
}

func newFakeStream(err error) *fakeStream {
	return &fakeStream{err: err, drained: make(chan struct{}), shutdown: make(chan struct{})}
}

func (f *fakeStream) StartDraining() { f.drainOnce.Do(func() { close(f.drained) }) }

func (f *fakeStream) Shutdown(ctx context.Context) error {
	close(f.shutdown)
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("shutdown context has no deadline")
	}
	return f.err
}

// TestServerDrainsAndShutsDownStreams: streams stop accepting when the
// drain starts, are shut down only after the drain delay, and their error
// is Serve's error.
func TestServerDrainsAndShutsDownStreams(t *testing.T) {
	t.Parallel()

	streamErr := errors.New("stream stuck")
	fs := newFakeStream(streamErr)
	ts := startTestServer(t, httpx.ServerOptions{
		DrainDelay:      time.Second,
		ShutdownTimeout: 2 * time.Second,
		Streams:         []httpx.Stream{fs},
	})

	ts.cancel()
	select {
	case <-fs.drained:
	case <-time.After(5 * time.Second):
		t.Fatal("StartDraining not called")
	}
	select {
	case <-fs.shutdown:
		t.Fatal("Shutdown called before the drain delay ended")
	default:
	}

	select {
	case err := <-ts.served:
		if !errors.Is(err, streamErr) {
			t.Fatalf("Serve() = %v, want the stream's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	select {
	case <-fs.shutdown:
	default:
		t.Fatal("Serve returned without shutting the stream down")
	}
}
