package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
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
		{"migrate without command", []string{"migrate"}, ExitUsage},
		{"migrate unknown command", []string{"migrate", "sideways"}, ExitUsage},
		{"migrate extra args", []string{"migrate", "up", "now"}, ExitUsage},
		{"migrate help", []string{"migrate", "help"}, ExitOK},
		{"maint without command", []string{"maint"}, ExitUsage},
		{"maint unknown command", []string{"maint", "vacuum"}, ExitUsage},
		{"maint help", []string{"maint", "help"}, ExitOK},
		{"staff without command", []string{"staff"}, ExitUsage},
		{"staff unknown command", []string{"staff", "delete"}, ExitUsage},
		{"staff create without role", []string{"staff", "create", "--email", "a@iskoala.com"}, ExitUsage},
		{"staff disable without email", []string{"staff", "disable"}, ExitUsage},
		{"staff help", []string{"staff", "help"}, ExitOK},
		{"bootstrap with args", []string{"bootstrap", "now"}, ExitUsage},
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

// TestCommandsRequireTheirDatabaseURL: each command fails closed, naming
// the variable it needs, before it touches a database.
func TestCommandsRequireTheirDatabaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args    []string
		wantVar string
	}{
		{[]string{"serve"}, "KOALA_DATABASE_URL"},
		{[]string{"migrate", "up"}, "KOALA_OWNER_DATABASE_URL"},
		{[]string{"migrate", "status"}, "KOALA_OWNER_DATABASE_URL"},
		{[]string{"maint", "reindex"}, "KOALA_OWNER_DATABASE_URL"},
		{[]string{"maint", "audit-purge"}, "KOALA_MAINT_DATABASE_URL"},
		{[]string{"bootstrap"}, "KOALA_BOOTSTRAP_DATABASE_URL"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			code, stdout, _ := run(tt.args, "KOALA_ENV=test")
			if code != ExitError {
				t.Fatalf("exit = %d, want %d", code, ExitError)
			}
			if !strings.Contains(stdout, tt.wantVar+": required") {
				t.Fatalf("stdout = %q, want an error naming %s", stdout, tt.wantVar)
			}
		})
	}
}

// TestServeNeedsEdgeVariablesInProduction: in production serve refuses to
// start without the minimum app version, the trusted proxies and the two
// token signing key rings and the login pepper (ADR-0025), before it
// dials the database (the unreachable URL is never used).
func TestServeNeedsEdgeVariablesInProduction(t *testing.T) {
	t.Parallel()
	code, stdout, _ := run([]string{"serve"},
		"KOALA_ENV=production", "KOALA_DATABASE_URL=postgres://app@db.invalid/koala?sslmode=verify-full")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	for _, v := range []string{"KOALA_MIN_APP_VERSION", "KOALA_TRUSTED_PROXIES", "KOALA_JWT_MARKETPLACE_KEYS", "KOALA_JWT_STAFF_KEYS", "KOALA_LOGIN_PEPPER"} {
		if !strings.Contains(stdout, v+": required by serve in production") {
			t.Errorf("stdout = %q, want an error naming %s", stdout, v)
		}
	}
}

// TestMigrateDownRefusedInProduction: the refusal comes before any
// connection, so the unreachable URL is never dialed.
func TestMigrateDownRefusedInProduction(t *testing.T) {
	t.Parallel()

	code, stdout, _ := run([]string{"migrate", "down"},
		"KOALA_ENV=production", "KOALA_OWNER_DATABASE_URL=postgres://migrator@db.invalid/koala?sslmode=verify-full")
	if code != ExitError || !strings.Contains(stdout, "down is refused in production") {
		t.Fatalf("exit %d, stdout %q", code, stdout)
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
	metricsLn, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	environ := []string{
		"KOALA_ENV=test", "KOALA_HTTP_ADDR=" + ln.Addr().String(), "KOALA_SHUTDOWN_DRAIN_DELAY=0s",
		"KOALA_METRICS_ADDR=" + metricsLn.Addr().String(),
	}
	cfg, err := config.Load(environ)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := log.New(&logs, slog.LevelInfo)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	database := lazyDB(t)
	go func() {
		done <- serve(ctx, cfg, logger, "v0.0.0-test", listeners{api: ln, metrics: metricsLn}, database)
	}()

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

	// The metrics listener serves the registry the router records in; the
	// healthcheck's /livez request is in it, by route template.
	body := httpGet(t, "http://"+metricsLn.Addr().String()+"/metrics")
	for _, want := range []string{
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/livez"}`,
		`koala_build_info{version="v0.0.0-test"} 1`,
		"go_goroutines ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
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

func httpGet(t *testing.T, url string) string {
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
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d, %v", url, resp.StatusCode, err)
	}
	return string(b)
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

// koala keygen prints secrets the configuration accepts (ADR-0025 §2); it
// works on any machine without openssl or GNU coreutils.
func TestKeygen(t *testing.T) {
	t.Parallel()

	code, stdout, _ := run([]string{"keygen", "jwt", "mp-2026-11"})
	if code != ExitOK {
		t.Fatalf("keygen jwt: exit %d", code)
	}
	ring, err := tokenkeys.Parse(strings.TrimSpace(stdout))
	if err != nil {
		t.Fatalf("keygen jwt output does not parse: %v", err)
	}
	if k, _ := ring.Signer(); k.ID() != "mp-2026-11" {
		t.Errorf("kid = %q", k.ID())
	}
	code, stdout, _ = run([]string{"keygen", "pepper"})
	if code != ExitOK {
		t.Fatalf("keygen pepper: exit %d", code)
	}
	if _, err := tokenkeys.ParsePepper(strings.TrimSpace(stdout)); err != nil {
		t.Fatalf("keygen pepper output does not parse: %v", err)
	}
	for _, args := range [][]string{{"keygen"}, {"keygen", "jwt"}, {"keygen", "jwt", "bad kid"}, {"keygen", "rsa"}} {
		if code, _, _ := run(args); code != ExitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, ExitUsage)
		}
	}
}
