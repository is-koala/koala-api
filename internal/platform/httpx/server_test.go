package httpx_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/httpx"
)

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	failing := false
	health := httpx.NewHealth(logger, func(context.Context) error {
		if failing {
			return errors.New("db down")
		}
		return nil
	})
	h := newRouter(t, logger, health)

	if rec := do(h, http.MethodGet, "/livez", nil); rec.Code != http.StatusOK {
		t.Fatalf("/livez = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d", rec.Code)
	}

	failing = true
	if rec := do(h, http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with failing check = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/livez", nil); rec.Code != http.StatusOK {
		t.Fatalf("/livez must ignore readiness checks, got %d", rec.Code)
	}

	failing = false
	health.StartDraining()
	if rec := do(h, http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while draining = %d", rec.Code)
	}
}

// testServer is a running httpx.Server with a /slow route that blocks
// until release is called. Cleanup releases the handler, stops the server
// and waits for Serve to return, also when the test fails early.
type testServer struct {
	base    string
	client  *http.Client
	cancel  context.CancelFunc
	served  chan error
	started chan struct{}
	release func()
}

func startTestServer(t *testing.T, opts httpx.ServerOptions) *testServer {
	t.Helper()
	logger, _ := newLogger()
	health := httpx.NewHealth(logger)
	router := newRouter(t, logger, health)

	started := make(chan struct{})
	var startOnce sync.Once
	unblock := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(unblock) }) }
	router.Get("/slow", func(w http.ResponseWriter, _ *http.Request) {
		startOnce.Do(func() { close(started) })
		<-unblock
		_, _ = io.WriteString(w, "done")
	})

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httpx.NewServer(withClient(router), health, logger, opts)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		served <- srv.Serve(ctx, ln)
		close(done)
	}()
	t.Cleanup(func() {
		release()
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return during cleanup")
		}
	})

	return &testServer{
		base:    "http://" + ln.Addr().String(),
		client:  &http.Client{Timeout: 5 * time.Second},
		cancel:  cancel,
		served:  served,
		started: started,
		release: release,
	}
}

// TestServerShutdownOrder checks plan §3.10: on cancel, readiness turns
// 503 while the listener still accepts, in-flight requests finish, then
// Serve returns nil.
func TestServerShutdownOrder(t *testing.T) {
	t.Parallel()

	ts := startTestServer(t, httpx.ServerOptions{DrainDelay: time.Second, ShutdownTimeout: 5 * time.Second})

	if code := get(t, ts.client, ts.base+"/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz before shutdown = %d", code)
	}

	slow := make(chan int, 1)
	go func() { slow <- get(t, ts.client, ts.base+"/slow") }()
	<-ts.started

	ts.cancel()

	// During the drain delay the listener is open and readiness is 503.
	deadline := time.Now().Add(800 * time.Millisecond)
	for {
		code := get(t, ts.client, ts.base+"/readyz")
		if code == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz never turned 503 during drain (last %d)", code)
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case err := <-ts.served:
		t.Fatalf("Serve returned with a request in flight: %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	ts.release()
	if code := <-slow; code != http.StatusOK {
		t.Fatalf("in-flight request = %d, want 200", code)
	}
	select {
	case err := <-ts.served:
		if err != nil {
			t.Fatalf("Serve() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}

	if _, err := fetch(ts.client, ts.base+"/livez"); err == nil {
		t.Fatal("server still accepts connections after shutdown")
	}
}

// TestServerForceClosesAfterShutdownTimeout checks the other exit: a
// request that outlives ShutdownTimeout is cut off and Serve reports it.
func TestServerForceClosesAfterShutdownTimeout(t *testing.T) {
	t.Parallel()

	ts := startTestServer(t, httpx.ServerOptions{DrainDelay: 0, ShutdownTimeout: 200 * time.Millisecond})

	slow := make(chan error, 1)
	go func() {
		_, err := fetch(ts.client, ts.base+"/slow")
		slow <- err
	}()
	<-ts.started

	ts.cancel()

	select {
	case err := <-ts.served:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Serve() = %v, want a wrapped context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the shutdown timeout")
	}
	if err := <-slow; err == nil {
		t.Fatal("the in-flight request completed; want its connection closed")
	}
}

// fetch performs a GET and drains the body.
func fetch(client *http.Client, url string) (int, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// get is fetch that reports transport errors on t.
func get(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	code, err := fetch(client, url)
	if err != nil {
		t.Error(err)
	}
	return code
}
