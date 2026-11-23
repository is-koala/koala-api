package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"text/tabwriter"

	"github.com/is-koala/koala-api/db/referencedata"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/log"
)

const referenceUsage = `usage: koala reference sync [--dry-run]

  sync            bring the reference tables (countries, cities, districts,
                  universities, faculties, departments) in line with the
                  files built into this binary (db/referencedata): insert
                  new rows, update changed ones, retire dropped ones; ids
                  never change and no row is deleted (ADR-0037). Running
                  it again changes nothing. koala migrate up runs it after
                  the migrations.
  sync --dry-run  print what sync would do and roll it back

Both print one line per table: inserted, updated, retired, unchanged.
Connects with KOALA_OWNER_DATABASE_URL, a login that is a member of
koala_owner.
`

// errDryRun rolls the dry run's transaction back after Sync counted.
var errDryRun = errors.New("reference sync: dry run")

// runReference runs `koala reference`.
func runReference(ctx context.Context, args []string, sio IO) int {
	dryRun := false
	switch {
	case len(args) == 1 && args[0] == "sync":
	case len(args) == 2 && args[0] == "sync" && args[1] == "--dry-run":
		dryRun = true
	case len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help"):
		_, _ = fmt.Fprint(sio.Stdout, referenceUsage)
		return ExitOK
	default:
		_, _ = fmt.Fprint(sio.Stderr, referenceUsage)
		return ExitUsage
	}

	cfg, err := config.Load(sio.Environ)
	if err != nil {
		log.New(sio.Stderr, slog.LevelInfo).ErrorContext(ctx, "reference sync failed", slog.Any("error", err))
		return ExitError
	}
	logger := log.New(sio.Stdout, cfg.LogLevel)
	if cfg.OwnerDatabaseURL == "" {
		logger.ErrorContext(ctx, "reference sync failed", slog.String("error", config.VarOwnerDatabaseURL+": required by reference sync"))
		return ExitError
	}
	summary, err := syncReference(ctx, cfg.OwnerDatabaseURL.Reveal(), logger, dryRun)
	if err != nil {
		logger.ErrorContext(ctx, "reference sync failed", slog.Any("error", err))
		return ExitError
	}
	if err := writeSummary(sio.Stdout, summary, dryRun); err != nil {
		return ExitError
	}
	return ExitOK
}

// syncReference runs referencedata.Sync in one koala_owner transaction:
// committed, or rolled back after counting when dryRun is set. A dry run
// takes the same path as the real one, so it cannot report what the real
// one would not do.
func syncReference(ctx context.Context, ownerURL string, logger *slog.Logger, dryRun bool) (referencedata.Summary, error) {
	data, err := referencedata.Load()
	if err != nil {
		return nil, fmt.Errorf("reference sync: %w", err)
	}
	owner, err := db.Open(ctx, db.Config{URL: ownerURL, Role: db.RoleOwner, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("reference sync: %w", err)
	}
	defer owner.Close()
	var summary referencedata.Summary
	err = owner.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		s, err := referencedata.Sync(ctx, q, data)
		if err != nil {
			return fmt.Errorf("reference sync: %w", err)
		}
		summary = s
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return nil, fmt.Errorf("reference sync: %w", err)
	}
	return summary, nil
}

// writeSummary prints one line per table.
func writeSummary(w io.Writer, s referencedata.Summary, dryRun bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if dryRun {
		_, _ = fmt.Fprintln(tw, "dry run: nothing was written")
	}
	_, _ = fmt.Fprintln(tw, "TABLE\tINSERTED\tUPDATED\tRETIRED\tUNCHANGED")
	for _, c := range s {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\n", c.Table, c.Inserted, c.Updated, c.Retired, c.Unchanged)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}
