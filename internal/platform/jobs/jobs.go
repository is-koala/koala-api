// Package jobs builds the River client (ADR-0010) with Koala's defaults.
//
// A job that must not be lost is inserted in the transaction that writes
// its data:
//
//	err := database.InTx(ctx, func(ctx context.Context, q db.Querier) error {
//		// ... write the data through q ...
//		_, err := client.InsertTx(ctx, q.PgxTx(), args, nil)
//		return err
//	})
//
// Both commit together or neither does. Periodic jobs run only on the
// elected leader among all clients on the database, so each fires once per
// schedule however many instances run.
package jobs

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Config is what varies between Koala's River clients.
type Config struct {
	// ID identifies the client in River's tables, for example in
	// river_leader. Empty lets River derive one from the host name.
	ID string
	// Logger receives River's logs. Required.
	Logger *slog.Logger
	// Queues to work, with their concurrency. Empty makes an insert-only
	// client: it can insert jobs but works none and is never leader.
	Queues map[string]river.QueueConfig
	// Workers for every job kind the client works or inserts.
	Workers *river.Workers
	// PeriodicJobs are enqueued by the leader only.
	PeriodicJobs []*river.PeriodicJob
}

// Client is the River client type of this codebase: pgx v5 transactions.
type Client = river.Client[pgx.Tx]

// NewClient returns a River client on pool. The pool's connections must act
// as koala_app (db.Open with db.RoleApp); 0009_river_grants.sql gives that
// role what River needs.
//
// River's in-process reindexer is turned off: REINDEX needs table
// ownership, which koala_app deliberately lacks (ADR-0013). `koala maint
// reindex` (ReindexRiver) rebuilds the indexes as koala_owner instead
// (ADR-0017).
func NewClient(pool *pgxpool.Pool, cfg Config) (*Client, error) {
	if cfg.Logger == nil {
		return nil, errors.New("jobs: Logger is required")
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		ID:                cfg.ID,
		Logger:            cfg.Logger,
		Queues:            cfg.Queues,
		Workers:           cfg.Workers,
		PeriodicJobs:      cfg.PeriodicJobs,
		ReindexerSchedule: river.NeverSchedule(),
	})
	if err != nil {
		return nil, fmt.Errorf("create river client: %w", err)
	}
	return client, nil
}
