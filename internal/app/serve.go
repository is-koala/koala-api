package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/jobs"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/platform/metrics"
)

func runServe(ctx context.Context, args []string, sio IO) int {
	if rejectArgs("serve", args, sio) {
		return ExitUsage
	}
	cfg, err := config.Load(sio.Environ)
	if err != nil {
		log.New(sio.Stderr, slog.LevelInfo).ErrorContext(ctx, "startup failed", slog.Any("error", err))
		return ExitError
	}
	logger := log.New(sio.Stdout, cfg.LogLevel)
	if cfg.DatabaseURL == "" {
		logger.ErrorContext(ctx, "startup failed", slog.String("error", config.VarDatabaseURL+": required by serve"))
		return ExitError
	}
	if err := errors.Join(cfg.ServeLoginErrors(), cfg.ServeEdgeErrors(), cfg.ServeAuthErrors()); err != nil {
		logger.ErrorContext(ctx, "startup failed", slog.Any("error", err))
		return ExitError
	}

	database, err := db.Open(ctx, db.Config{URL: cfg.DatabaseURL.Reveal(), Role: db.RoleApp, Logger: logger})
	if err != nil {
		logger.ErrorContext(ctx, "startup failed", slog.Any("error", err))
		return ExitError
	}
	// Closed after serve returns, that is after the HTTP drain (plan §3.10).
	defer database.Close()

	// A login that could leave koala_app is refused at startup (ADR-0017).
	// Any other failure only means the database is not reachable yet:
	// start anyway and let /readyz report it.
	err = database.Ready(ctx)
	if errors.Is(err, db.ErrLoginTooPrivileged) {
		logger.ErrorContext(ctx, "startup failed", slog.Any("error", err))
		return ExitError
	}
	if err != nil {
		logger.WarnContext(ctx, "database not ready at startup", slog.Any("error", err))
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		logger.ErrorContext(ctx, "startup failed", slog.Any("error", err))
		return ExitError
	}
	metricsLn, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.MetricsAddr)
	if err != nil {
		_ = ln.Close()
		logger.ErrorContext(ctx, "startup failed", slog.Any("error", err))
		return ExitError
	}
	if err := serve(ctx, cfg, logger, sio.Version, listeners{api: ln, metrics: metricsLn}, database, database.Ready); err != nil {
		logger.ErrorContext(ctx, "server failed", slog.Any("error", err))
		return ExitError
	}
	return ExitOK
}

// listeners are the sockets serve owns: the API and the internal metrics
// port (plan §3.10).
type listeners struct {
	api     net.Listener
	metrics net.Listener
}

// serve wires the application and blocks until ctx is canceled and the
// shutdown sequence has finished. Order on shutdown (plan §3.10):
// readiness 503 → HTTP drain → stop workers → metrics listener
// → close pools; the caller closes the pools after serve returns. The
// metrics listener stays up through the drain, so the drain itself is
// observable. checks gate /readyz.
func serve(ctx context.Context, cfg config.Config, logger *slog.Logger, version string, lns listeners,
	database *db.DB, checks ...httpx.ReadinessCheck,
) error {
	logger.InfoContext(ctx, "starting",
		slog.String("version", version),
		slog.String("env", string(cfg.Env)))

	reg, err := metrics.NewRegistry(version)
	if err != nil {
		_ = lns.api.Close()
		_ = lns.metrics.Close()
		return fmt.Errorf("serve: %w", err)
	}
	metricsCtx, stopMetrics := context.WithCancel(context.WithoutCancel(ctx))
	defer stopMetrics()
	metricsDone := make(chan error, 1)
	go func() { metricsDone <- metrics.Serve(metricsCtx, lns.metrics, metrics.Handler(reg, logger), logger) }()

	health := httpx.NewHealth(logger, checks...)
	mods, router, wm, err := wireService(ctx, cfg, database, clock.Real{}, logger, health, reg)
	var workers *jobs.Client
	if err == nil {
		workers, err = newWorkerClient(database, mods, logger)
	}
	stopWorkers := func(context.Context) {}
	if err == nil {
		stopWorkers = startWorkers(ctx, workers, logger, wm, workerRetry)
	}
	if err == nil {
		srv := httpx.NewServer(router, health, logger, httpx.ServerOptions{
			DrainDelay:      cfg.ShutdownDrainDelay,
			ShutdownTimeout: cfg.ShutdownTimeout,
			// The hub owns the hijacked WebSocket connections: drain
			// refuses new ones, shutdown closes the rest (ADR-0018 §8).
			Streams: []httpx.Stream{mods.hub},
		})
		if serveErr := srv.Serve(ctx, lns.api); serveErr != nil {
			err = fmt.Errorf("serve: %w", serveErr)
		}
	} else {
		_ = lns.api.Close()
	}
	// After the drain: cancel the workers and wait for them within
	// workerStopTimeout (the margin of stop_grace_period over drain +
	// shutdown, ADR-0025 §7).
	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), workerStopTimeout)
	stopWorkers(stopCtx)
	cancelStop()
	stopMetrics()
	if metricsErr := <-metricsDone; metricsErr != nil && err == nil {
		err = metricsErr
	}
	if err != nil {
		return err
	}
	logger.InfoContext(context.WithoutCancel(ctx), "stopped")
	return nil
}

// wireService wires the modules and the router and registers every metric
// serve exports on reg: the modules', the router's and the workers'.
// TestAlertRulesUseRegisteredMetrics reads the same registry.
func wireService(ctx context.Context, cfg config.Config, database *db.DB, clk clock.Clock, logger *slog.Logger,
	health *httpx.Health, reg prometheus.Registerer,
) (modules, *httpx.Router, *workerMetrics, error) {
	mods, err := wireModules(ctx, cfg, database, clk, logger, reg)
	if err != nil {
		return modules{}, nil, nil, err
	}
	router, err := newRouter(ctx, cfg, logger, health, reg, rateLimitGroups(), mods)
	if err != nil {
		return modules{}, nil, nil, err
	}
	wm, err := newWorkerMetrics(reg)
	if err != nil {
		return modules{}, nil, nil, err
	}
	return mods, router, wm, nil
}
