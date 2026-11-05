package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/log"
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
	if err := serve(ctx, cfg, logger, sio.Version, ln, database.Ready); err != nil {
		logger.ErrorContext(ctx, "server failed", slog.Any("error", err))
		return ExitError
	}
	return ExitOK
}

// serve wires the application and blocks until ctx is canceled and the
// shutdown sequence has finished. Order on shutdown (plan §3.10):
// readiness 503 → HTTP drain → (Faz 1) cancel workers → close pools; the
// caller closes the pools after serve returns. checks gate /readyz.
func serve(ctx context.Context, cfg config.Config, logger *slog.Logger, version string, ln net.Listener, checks ...httpx.ReadinessCheck) error {
	logger.InfoContext(ctx, "starting",
		slog.String("version", version),
		slog.String("env", string(cfg.Env)))

	health := httpx.NewHealth(logger, checks...)
	router := httpx.NewRouter(logger, health)
	if err := mountAPI(ctx, router, logger, cfg.Env); err != nil {
		return err
	}

	srv := httpx.NewServer(router, health, logger, httpx.ServerOptions{
		DrainDelay:      cfg.ShutdownDrainDelay,
		ShutdownTimeout: cfg.ShutdownTimeout,
	})
	if err := srv.Serve(ctx, ln); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	// Faz 1: cancel the worker context, wait for workers, close pools.
	logger.InfoContext(context.WithoutCancel(ctx), "stopped")
	return nil
}
