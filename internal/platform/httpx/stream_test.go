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
		DrainDelay:      300 * time.Millisecond,
		ShutdownTimeout: time.Second,
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
