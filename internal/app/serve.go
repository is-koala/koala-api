package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/is-koala/koala-api/internal/platform/config"
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

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		logger.ErrorContext(ctx, "startup failed", slog.Any("error", err))
		return ExitError
	}
	if err := serve(ctx, cfg, logger, sio.Version, ln); err != nil {
		logger.ErrorContext(ctx, "server failed", slog.Any("error", err))
		return ExitError
	}
	return ExitOK
}

// serve wires the application and blocks until ctx is canceled and the
// shutdown sequence has finished. Order on shutdown (plan §3.10):
// readiness 503 → HTTP drain → (Faz 1) cancel workers → close pools.
func serve(ctx context.Context, cfg config.Config, logger *slog.Logger, version string, ln net.Listener) error {
	logger.InfoContext(ctx, "starting",
		slog.String("version", version),
		slog.String("env", string(cfg.Env)))

	// Faz 1: open the pgx pool here and pass its Ping as a readiness check.
	health := httpx.NewHealth(logger)
	router := httpx.NewRouter(logger, health)
	if err := mountAPI(ctx, router, logger); err != nil {
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
