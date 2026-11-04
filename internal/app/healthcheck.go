package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/is-koala/koala-api/internal/platform/config"
)

const healthcheckTimeout = 3 * time.Second

// runHealthcheck lets the distroless image (no curl or wget) probe itself:
// HEALTHCHECK CMD ["/koala", "healthcheck"]. It reads the same KOALA_*
// environment as serve and asks /readyz on the loopback interface.
func runHealthcheck(ctx context.Context, args []string, sio IO) int {
	if rejectArgs("healthcheck", args, sio) {
		return ExitUsage
	}
	cfg, err := config.Load(sio.Environ)
	if err != nil {
		_, _ = fmt.Fprintf(sio.Stderr, "healthcheck: %v\n", err)
		return ExitError
	}
	url, err := readyURL(cfg.HTTPAddr)
	if err != nil {
		_, _ = fmt.Fprintf(sio.Stderr, "healthcheck: %v\n", err)
		return ExitError
	}

	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		_, _ = fmt.Fprintf(sio.Stderr, "healthcheck: %v\n", err)
		return ExitError
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(sio.Stderr, "healthcheck: %v\n", err)
		return ExitError
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(sio.Stderr, "healthcheck: /readyz returned %d\n", resp.StatusCode)
		return ExitError
	}
	return ExitOK
}

// readyURL maps the listen address to a URL on loopback when the server
// listens on all interfaces.
func readyURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("parse listen address: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/readyz", nil
}
