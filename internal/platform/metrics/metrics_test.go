package metrics_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/metrics"
)

// The listener serves the Go runtime, process and build metrics on
// /metrics only, and stops when its context ends.
func TestServeRegistry(t *testing.T) {
	t.Parallel()

	reg, err := metrics.NewRegistry("v1.2.3-test")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- metrics.Serve(ctx, ln, metrics.Handler(reg, logger), logger) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve() = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return")
		}
	})

	base := "http://" + ln.Addr().String()
	body := get(t, base+metrics.Path, http.StatusOK)
	want := []string{`koala_build_info{version="v1.2.3-test"} 1`, "go_goroutines ", "go_info{"}
	if runtime.GOOS == "linux" {
		want = append(want, "process_resident_memory_bytes ")
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("/metrics lacks %q", w)
		}
	}
	get(t, base+"/", http.StatusNotFound)
}

func get(t *testing.T, url string, wantStatus int) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s = %d, want %d", url, resp.StatusCode, wantStatus)
	}
	return string(b)
}
