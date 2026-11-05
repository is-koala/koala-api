package app

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/db/bootstrap"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/log"
)

// runBootstrap creates the group roles (db/bootstrap/roles.sql). It is
// idempotent and runs in the release before `koala migrate up`
// (scripts/release-db.sh, ADR-0017). The login needs CREATEROLE (and, for
// roles that already exist, ADMIN on them) and must not be a superuser; it
// is used for nothing else.
func runBootstrap(ctx context.Context, args []string, sio IO) int {
	if rejectArgs("bootstrap", args, sio) {
		return ExitUsage
	}
	cfg, err := config.Load(sio.Environ)
	if err != nil {
		log.New(sio.Stderr, slog.LevelInfo).ErrorContext(ctx, "bootstrap failed", slog.Any("error", err))
		return ExitError
	}
	logger := log.New(sio.Stdout, cfg.LogLevel)
	if cfg.BootstrapDatabaseURL == "" {
		logger.ErrorContext(ctx, "bootstrap failed", slog.String("error", config.VarBootstrapDatabaseURL+": required by bootstrap"))
		return ExitError
	}

	connCfg, err := pgx.ParseConfig(cfg.BootstrapDatabaseURL.Reveal())
	if err != nil {
		logger.ErrorContext(ctx, "bootstrap failed", slog.String("error", "parse database URL: invalid connection string"))
		return ExitError
	}
	conn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		logger.ErrorContext(ctx, "bootstrap failed", slog.Any("error", err))
		return ExitError
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if err := db.CheckNotSuperuser(ctx, conn); err != nil {
		logger.ErrorContext(ctx, "bootstrap failed", slog.Any("error", err))
		return ExitError
	}
	if _, err := conn.Exec(ctx, bootstrap.RolesSQL); err != nil {
		logger.ErrorContext(ctx, "bootstrap failed", slog.Any("error", err))
		return ExitError
	}
	logger.InfoContext(ctx, "group roles present")
	return ExitOK
}
