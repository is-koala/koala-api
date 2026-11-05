package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/jobs"
	"github.com/is-koala/koala-api/internal/platform/log"
)

const maintUsage = `usage: koala maint <reindex>

  reindex  rebuild River's job indexes concurrently, dropping invalid
           leftovers of interrupted rebuilds first; scheduled weekly

Connects with KOALA_OWNER_DATABASE_URL, a login that is a member of
koala_owner: REINDEX needs ownership (ADR-0017).
`

// runMaint runs a maintenance task. These are one-off processes started
// by a scheduled ops job, not by the server.
func runMaint(ctx context.Context, args []string, sio IO) int {
	if len(args) != 1 {
		_, _ = fmt.Fprint(sio.Stderr, maintUsage)
		return ExitUsage
	}
	switch args[0] {
	case "reindex":
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(sio.Stdout, maintUsage)
		return ExitOK
	default:
		_, _ = fmt.Fprintf(sio.Stderr, "koala maint: unknown command %q\n\n%s", args[0], maintUsage)
		return ExitUsage
	}

	cfg, err := config.Load(sio.Environ)
	if err != nil {
		log.New(sio.Stderr, slog.LevelInfo).ErrorContext(ctx, "maint failed", slog.Any("error", err))
		return ExitError
	}
	logger := log.New(sio.Stdout, cfg.LogLevel)
	if cfg.OwnerDatabaseURL == "" {
		logger.ErrorContext(ctx, "maint failed", slog.String("error", config.VarOwnerDatabaseURL+": required by maint"))
		return ExitError
	}

	database, err := db.Open(ctx, db.Config{URL: cfg.OwnerDatabaseURL.Reveal(), Role: db.RoleOwner, Logger: logger})
	if err != nil {
		logger.ErrorContext(ctx, "maint failed", slog.Any("error", err))
		return ExitError
	}
	defer database.Close()

	if err := jobs.ReindexRiver(ctx, database.Pool(), logger); err != nil {
		logger.ErrorContext(ctx, "maint failed", slog.Any("error", err))
		return ExitError
	}
	logger.InfoContext(ctx, "maint done", slog.String("task", "reindex"))
	return ExitOK
}
