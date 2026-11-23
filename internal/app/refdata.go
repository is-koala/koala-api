package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"text/tabwriter"

	"github.com/is-koala/koala-api/db/referencedata"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/log"
)

const referenceUsage = `usage: koala reference sync [--dry-run] [--allow-retire=<table>:<rows>@<version>[,...]]

  sync            bring the reference tables (countries, cities, districts,
                  universities, faculties, departments) in line with the
                  files built into this binary (db/referencedata): insert
                  new rows, update changed ones, retire dropped ones; ids
                  never change and no row is deleted (ADR-0037). Running
                  it again changes nothing. koala migrate up runs it after
                  the migrations.
  --dry-run       print what sync would do and roll it back
  --allow-retire  let sync retire more than the limit of a table (5 % of
                  its rows in use, at least 10), up to <rows>, when it
                  syncs data version <version>; without it such a sync
                  writes nothing and names the table. An allowance for
                  another version is ignored with a warning. migrate up
                  reads the same from KOALA_REFERENCE_ALLOW_RETIRE.

The data version: a database that holds a newer one than this binary
(a rolled-back release) is left as it is, with a warning.

Prints one line per table (inserted, updated, retired, unchanged), then
every row it writes: "+" inserted, "~" updated (old -> new name),
"^" brought back into use, "-" retired.

Connects with KOALA_OWNER_DATABASE_URL, a login that is a member of
koala_owner.
`

// errDryRun rolls the dry run's transaction back after Sync planned and
// wrote.
var errDryRun = errors.New("reference sync: dry run")

// runReference runs `koala reference`.
func runReference(ctx context.Context, args []string, sio IO) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		_, _ = fmt.Fprint(sio.Stdout, referenceUsage)
		return ExitOK
	}
	if len(args) == 0 || args[0] != "sync" {
		_, _ = fmt.Fprint(sio.Stderr, referenceUsage)
		return ExitUsage
	}
	fs := flag.NewFlagSet("reference sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "")
	allow := fs.String("allow-retire", "", "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		_, _ = fmt.Fprint(sio.Stderr, referenceUsage)
		return ExitUsage
	}
	allowRetire, err := referencedata.ParseAllowRetire(*allow)
	if err != nil {
		_, _ = fmt.Fprintf(sio.Stderr, "koala reference sync: %v\n\n%s", err, referenceUsage)
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
	res, err := syncReference(ctx, cfg.OwnerDatabaseURL.Reveal(), logger, referencedata.Options{AllowRetire: allowRetire}, *dryRun)
	if err != nil {
		logger.ErrorContext(ctx, "reference sync failed", slog.Any("error", err))
		return ExitError
	}
	if err := writeResult(sio.Stdout, res, *dryRun); err != nil {
		return ExitError
	}
	return ExitOK
}

// syncReference runs referencedata.Sync in one koala_owner transaction:
// committed, or rolled back when dryRun is set. A dry run takes the same
// path as the real one, writes included, so it cannot report what the
// real one would not do. A database that holds a newer data version is
// left as it is and logged as a warning (ADR-0037: a rolled-back release
// does not take the data back).
func syncReference(ctx context.Context, ownerURL string, logger *slog.Logger, opts referencedata.Options, dryRun bool,
) (referencedata.Result, error) {
	data, err := referencedata.Load()
	if err != nil {
		return referencedata.Result{}, fmt.Errorf("reference sync: %w", err)
	}
	owner, err := db.Open(ctx, db.Config{URL: ownerURL, Role: db.RoleOwner, Logger: logger})
	if err != nil {
		return referencedata.Result{}, fmt.Errorf("reference sync: %w", err)
	}
	defer owner.Close()
	var res referencedata.Result
	err = owner.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		r, err := referencedata.Sync(ctx, q, data, opts)
		if err != nil {
			return fmt.Errorf("reference sync: %w", err)
		}
		res = r
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return referencedata.Result{}, fmt.Errorf("reference sync: %w", err)
	}
	if res.Skipped {
		logger.WarnContext(ctx, "reference data left as it is: the database holds a newer version",
			slog.Int("data_version", res.DataVersion), slog.Int("database_version", res.DatabaseVersion))
	}
	for _, table := range slices.Sorted(maps.Keys(res.Ignored)) {
		logger.WarnContext(ctx, "reference allowance ignored: it names another data version",
			slog.String("table", table), slog.String("allowance", res.Ignored[table].String()),
			slog.Int("data_version", res.DataVersion))
	}
	return res, nil
}

// writeResult prints the summary and then every row written.
func writeResult(w io.Writer, res referencedata.Result, dryRun bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if dryRun {
		_, _ = fmt.Fprintln(tw, "dry run: nothing was written")
	}
	_, _ = fmt.Fprintf(tw, "data version %d, database version %d\n", res.DataVersion, res.DatabaseVersion)
	if res.Skipped {
		_, _ = fmt.Fprintln(tw, "skipped: the database holds a newer version; nothing was written")
	}
	_, _ = fmt.Fprintln(tw, "TABLE\tINSERTED\tUPDATED\tRETIRED\tUNCHANGED")
	for _, t := range res.Tables {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\n", t.Table, len(t.Inserted), len(t.Updated), len(t.Retired), t.Unchanged)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	for _, t := range res.Tables {
		for _, c := range t.Inserted {
			_, _ = fmt.Fprintf(w, "+ %s %s %q\n", t.Table, c.ID, c.Name)
		}
		for _, c := range t.Updated {
			switch {
			case c.Restored && c.OldName != "":
				_, _ = fmt.Fprintf(w, "^ %s %s %q -> %q\n", t.Table, c.ID, c.OldName, c.Name)
			case c.Restored:
				_, _ = fmt.Fprintf(w, "^ %s %s %q\n", t.Table, c.ID, c.Name)
			case c.OldName != "":
				_, _ = fmt.Fprintf(w, "~ %s %s %q -> %q\n", t.Table, c.ID, c.OldName, c.Name)
			}
			if c.OldCode != "" {
				_, _ = fmt.Fprintf(w, "~ %s %s code %s -> %s\n", t.Table, c.ID, c.OldCode, c.Code)
			}
		}
		for _, c := range t.Retired {
			_, _ = fmt.Fprintf(w, "- %s %s %q\n", t.Table, c.ID, c.Name)
		}
	}
	return nil
}
