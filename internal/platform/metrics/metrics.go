// Package metrics owns the service's Prometheus registry and the internal
// listener that publishes it (plan §3.10, ADR-0021). /metrics is served on
// its own port, never on the API's, and the proxy does not route to it.
//
// The registry is created here and passed down; nothing registers on the
// prometheus package's global default registry.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Path is where the metrics listener serves the registry.
const Path = "/metrics"

// MetricBuildInfo is 1, labeled with the build version.
const MetricBuildInfo = "koala_build_info"

// Listener timeouts. A scrape is small; nothing here is slow.
const (
	readHeaderTimeout = 5 * time.Second
	writeTimeout      = 30 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// NewRegistry returns a registry with the Go runtime and process
// collectors and koala_build_info.
func NewRegistry(version string) (*prometheus.Registry, error) {
	reg := prometheus.NewRegistry()
	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        MetricBuildInfo,
		Help:        "Always 1; the version label is the running build.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	build.Set(1)
	for _, c := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		build,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("metrics: %w", err)
		}
	}
	return reg, nil
}

// Handler serves reg in the Prometheus exposition format. Collection
// errors are logged and the rest of the registry is still served.
func Handler(reg *prometheus.Registry, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+Path, promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ErrorHandling: promhttp.ContinueOnError,
		Registry:      reg,
	}))
	return mux
}

// Serve serves h on ln until ctx is canceled, then shuts the listener
// down. It returns nil after a clean shutdown.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, logger *slog.Logger) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.InfoContext(ctx, "metrics listening", slog.String("addr", ln.Addr().String()))
	select {
	case err := <-served:
		return fmt.Errorf("metrics server: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("metrics shutdown: %w", err)
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("metrics server: %w", err)
	}
	return nil
}
