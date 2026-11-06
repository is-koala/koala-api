package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/is-koala/koala-api/internal/audit"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/jobs"
	"github.com/is-koala/koala-api/internal/platform/log"
)

const maintUsage = `usage: koala maint <reindex|audit-purge>

  reindex      rebuild River's job indexes concurrently, dropping invalid
               leftovers of interrupted rebuilds first; scheduled weekly
  audit-purge  delete audit trail rows past their retention: details
               after 6 months, entries after 24 months (FINANCIAL 10
               years), the PII access log after 24 months (ADR-0026);
               scheduled monthly

reindex connects with KOALA_OWNER_DATABASE_URL, a login that is a member
of koala_owner: REINDEX needs ownership (ADR-0017). audit-purge connects
with KOALA_MAINT_DATABASE_URL, a member of koala_maint: only that role may
delete audit rows, and only past their retention.
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
	case "audit-purge":
		return runAuditPurge(ctx, sio)
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

// runAuditPurge deletes the audit trail's rows past their retention as
// koala_maint (audit.Purge).
func runAuditPurge(ctx context.Context, sio IO) int {
	cfg, err := config.Load(sio.Environ)
	if err != nil {
		log.New(sio.Stderr, slog.LevelInfo).ErrorContext(ctx, "maint failed", slog.Any("error", err))
		return ExitError
	}
	logger := log.New(sio.Stdout, cfg.LogLevel)
	if cfg.MaintDatabaseURL == "" {
		logger.ErrorContext(ctx, "maint failed", slog.String("error", config.VarMaintDatabaseURL+": required by maint audit-purge"))
		return ExitError
	}
	maint, err := db.Open(ctx, db.Config{URL: cfg.MaintDatabaseURL.Reveal(), Role: db.RoleMaint, Logger: logger})
	if err != nil {
		logger.ErrorContext(ctx, "maint failed", slog.Any("error", err))
		return ExitError
	}
	defer maint.Close()
	p, err := audit.Purge(ctx, maint, clock.Real{})
	if err != nil {
		logger.ErrorContext(ctx, "maint failed", slog.Any("error", err))
		return ExitError
	}
	logger.InfoContext(ctx, "maint done", slog.String("task", "audit-purge"),
		slog.Int64("details", p.Details), slog.Int64("entries", p.Entries), slog.Int64("pii_access", p.PIIAccess))
	return ExitOK
}
