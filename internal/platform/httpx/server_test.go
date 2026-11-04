package httpx_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
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
	h := httpx.NewRouter(logger, health)

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

// TestServerShutdownOrder checks plan §3.10: on cancel, readiness turns
// 503 while the listener still accepts, in-flight requests finish, then
// Serve returns nil.
func TestServerShutdownOrder(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	health := httpx.NewHealth(logger)
	router := httpx.NewRouter(logger, health)

	started := make(chan struct{})
	release := make(chan struct{})
	router.Get("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	srv := httpx.NewServer(router, health, logger, httpx.ServerOptions{
		DrainDelay:      time.Second,
		ShutdownTimeout: 5 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()

	if code := get(t, client, base+"/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz before shutdown = %d", code)
	}

	slow := make(chan int, 1)
	go func() { slow <- get(t, client, base+"/slow") }()
	<-started

	cancel()

	// During the drain delay the listener is open and readiness is 503.
	deadline := time.Now().Add(800 * time.Millisecond)
	for {
		code := get(t, client, base+"/readyz")
		if code == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz never turned 503 during drain (last %d)", code)
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case err := <-served:
		t.Fatalf("Serve returned with a request in flight: %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	close(release)
	if code := <-slow; code != http.StatusOK {
		t.Fatalf("in-flight request = %d, want 200", code)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/livez", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("server still accepts connections after shutdown")
	}
}

func get(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Error(err)
		return 0
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Error(err)
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
