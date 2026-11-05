package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// reindexTimeout bounds one index rebuild. River's job table is small in
// steady state; an index that takes longer than this is a signal to look.
const reindexTimeout = 30 * time.Minute

// ReindexRiver rebuilds River's indexes with REINDEX INDEX CONCURRENTLY.
// It replaces River's in-process reindexer, which koala_app cannot run
// (ADR-0017): `koala maint reindex` calls it weekly on a pool that acts as
// koala_owner.
//
// A failed or interrupted concurrent rebuild leaves an INVALID index named
// <index>_ccnew or <index>_ccold (with an optional number). Such leftovers
// are dropped before the rebuild; dropping them needs ownership, which is
// why the MAINTAIN privilege is not enough. Only invalid leftovers of
// River's own index names are touched.
func ReindexRiver(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	for _, index := range river.ReindexerIndexNamesDefault() {
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", index).Scan(&exists); err != nil {
			return fmt.Errorf("look up index %s: %w", index, err)
		}
		if !exists {
			logger.WarnContext(ctx, "river index missing; migrations not applied?", slog.String("index", index))
			continue
		}

		rows, err := pool.Query(ctx, `
			SELECT c.relname
			FROM pg_class c
			JOIN pg_index i ON i.indexrelid = c.oid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema()
			  AND c.relname ~ ('^' || $1::text || '_cc(new|old)[0-9]*$')
			  AND NOT i.indisvalid`, index)
		if err != nil {
			return fmt.Errorf("list leftovers of %s: %w", index, err)
		}
		leftovers, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("list leftovers of %s: %w", index, err)
		}
		for _, leftover := range leftovers {
			if _, err := pool.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+pgx.Identifier{leftover}.Sanitize()); err != nil {
				return fmt.Errorf("drop leftover %s: %w", leftover, err)
			}
			logger.InfoContext(ctx, "dropped invalid reindex leftover", slog.String("index", leftover))
		}

		if err := reindexOne(ctx, pool, index); err != nil {
			return err
		}
		logger.InfoContext(ctx, "reindexed", slog.String("index", index))
	}
	return nil
}

func reindexOne(ctx context.Context, pool *pgxpool.Pool, index string) error {
	ctx, cancel := context.WithTimeout(ctx, reindexTimeout)
	defer cancel()
	if _, err := pool.Exec(ctx, "REINDEX INDEX CONCURRENTLY "+pgx.Identifier{index}.Sanitize()); err != nil {
		return fmt.Errorf("reindex %s: %w", index, err)
	}
	return nil
}
