package app

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"text/tabwriter"
	"time"

	"github.com/is-koala/koala-api/db/migrations"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/log"
)

const migrateUsage = `usage: koala migrate <up|down|status>

  up      apply every pending migration
  down    roll back the most recent migration; development only: refused
          when KOALA_ENV=production, River's down migrations drop job
          tables with their rows, NO TRANSACTION files are not atomic
  status  list migrations and whether they are applied

Connects with KOALA_OWNER_DATABASE_URL, a login that is a member of
koala_owner. Run koala bootstrap first (ADR-0017).
`

// runMigrate applies the embedded goose migrations (ADR-0012).
func runMigrate(ctx context.Context, args []string, sio IO) int {
	if len(args) != 1 {
		_, _ = fmt.Fprint(sio.Stderr, migrateUsage)
		return ExitUsage
	}
	sub := args[0]
	switch sub {
	case "up", "down", "status":
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(sio.Stdout, migrateUsage)
		return ExitOK
	default:
		_, _ = fmt.Fprintf(sio.Stderr, "koala migrate: unknown command %q\n\n%s", sub, migrateUsage)
		return ExitUsage
	}

	cfg, err := config.Load(sio.Environ)
	if err != nil {
		log.New(sio.Stderr, slog.LevelInfo).ErrorContext(ctx, "migrate failed", slog.Any("error", err))
		return ExitError
	}
	logger := log.New(sio.Stdout, cfg.LogLevel)
	if cfg.OwnerDatabaseURL == "" {
		logger.ErrorContext(ctx, "migrate failed", slog.String("error", config.VarOwnerDatabaseURL+": required by migrate"))
		return ExitError
	}
	if sub == "down" && cfg.Env == config.EnvProduction {
		// A down migration drops data. Production rolls forward with a new
		// migration or restores a backup.
		logger.ErrorContext(ctx, "migrate failed", slog.String("error", "down is refused in production"))
		return ExitError
	}

	migrator, err := db.NewMigrator(cfg.OwnerDatabaseURL.Reveal(), migrations.FS, logger)
	if err != nil {
		logger.ErrorContext(ctx, "migrate failed", slog.Any("error", err))
		return ExitError
	}
	defer func() {
		if err := migrator.Close(); err != nil {
			logger.WarnContext(ctx, "close migrator", slog.Any("error", err))
		}
	}()

	switch sub {
	case "up":
		res, err := migrator.Up(ctx)
		for _, r := range res {
			logMigration(ctx, logger, r.Direction, r.Source.Path, r.Duration)
		}
		if err != nil {
			logger.ErrorContext(ctx, "migrate failed", slog.Any("error", err))
			return ExitError
		}
		logger.InfoContext(ctx, "migrations up to date", slog.Int("applied", len(res)))
	case "down":
		res, err := migrator.Down(ctx)
		if err != nil {
			logger.ErrorContext(ctx, "migrate failed", slog.Any("error", err))
			return ExitError
		}
		logMigration(ctx, logger, res.Direction, res.Source.Path, res.Duration)
	case "status":
		st, err := migrator.Status(ctx)
		if err != nil {
			logger.ErrorContext(ctx, "migrate failed", slog.Any("error", err))
			return ExitError
		}
		w := tabwriter.NewWriter(sio.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "STATE\tMIGRATION\tAPPLIED AT")
		for _, s := range st {
			applied := "-"
			if !s.AppliedAt.IsZero() {
				applied = s.AppliedAt.UTC().Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", s.State, path.Base(s.Source.Path), applied)
		}
		if err := w.Flush(); err != nil {
			return ExitError
		}
	}
	return ExitOK
}

func logMigration(ctx context.Context, logger *slog.Logger, direction, file string, d time.Duration) {
	logger.InfoContext(ctx, "migration applied",
		slog.String("direction", direction),
		slog.String("file", path.Base(file)),
		slog.Duration("duration", d))
}
