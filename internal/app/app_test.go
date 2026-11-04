package app

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/log"
)

func run(args []string, environ ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, IO{Environ: environ, Stdout: &out, Stderr: &errOut, Version: "v0.0.0-test"})
	return code, out.String(), errOut.String()
}

func TestRunUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, ExitUsage},
		{"unknown command", []string{"launch"}, ExitUsage},
		{"serve with args", []string{"serve", "--port=1"}, ExitUsage},
		{"help", []string{"help"}, ExitOK},
		{"migrate placeholder", []string{"migrate"}, ExitError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if code, _, _ := run(tt.args); code != tt.want {
				t.Fatalf("exit = %d, want %d", code, tt.want)
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	t.Parallel()

	code, stdout, _ := run([]string{"version"})
	if code != ExitOK || strings.TrimSpace(stdout) != "v0.0.0-test" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
}

func TestServeRefusesInvalidConfig(t *testing.T) {
	t.Parallel()

	code, _, stderr := run([]string{"serve"}, "KOALA_ENV=prod")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "KOALA_ENV") || !strings.Contains(stderr, `"level":"ERROR"`) {
		t.Fatalf("stderr = %q, want a JSON error naming KOALA_ENV", stderr)
	}
}

func TestHealthcheckFailsWithoutServer(t *testing.T) {
	t.Parallel()

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if code, _, _ := run([]string{"healthcheck"}, "KOALA_ENV=test", "KOALA_HTTP_ADDR="+addr); code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
}

// TestServeAndHealthcheck runs the wired server and probes it with the
// healthcheck subcommand, as the Docker HEALTHCHECK does.
func TestServeAndHealthcheck(t *testing.T) {
	t.Parallel()

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	environ := []string{"KOALA_ENV=test", "KOALA_HTTP_ADDR=" + ln.Addr().String(), "KOALA_SHUTDOWN_DRAIN_DELAY=0s"}
	cfg, err := config.Load(environ)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := log.New(&logs, slog.LevelInfo)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, logger, "v0.0.0-test", ln) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		code, _, stderr := run([]string{"healthcheck"}, environ...)
		if code == ExitOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("healthcheck never passed: %s", stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func TestLivezURL(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		":8080":          "http://127.0.0.1:8080/livez",
		"0.0.0.0:8080":   "http://127.0.0.1:8080/livez",
		"[::]:8080":      "http://127.0.0.1:8080/livez",
		"127.0.0.1:9000": "http://127.0.0.1:9000/livez",
		"[::1]:9000":     "http://[::1]:9000/livez",
	}
	for addr, want := range tests {
		got, err := livezURL(addr)
		if err != nil || got != want {
			t.Errorf("livezURL(%q) = %q, %v; want %q", addr, got, err, want)
		}
	}
}
