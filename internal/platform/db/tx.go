package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrTxManaged is returned by Commit and Rollback on the transaction that
// Querier.PgxTx hands out: InTx alone ends the transaction.
var ErrTxManaged = errors.New("db: transaction is managed by InTx; return from the callback instead")

// ErrNestedTx is returned by InTx when ctx already belongs to an InTx
// callback. A nested call would take a second connection and run an
// independent transaction that commits even if the outer one rolls back.
// Pass the Querier down instead.
var ErrNestedTx = errors.New("db: InTx called inside an InTx callback; pass the Querier instead")

// txKey marks a context that belongs to an InTx callback.
type txKey struct{}

// statements is the part of pgx.Tx a store needs. It matches the DBTX
// interface sqlc generates, so a Querier can be passed to sqlc queries.
type statements interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, rows pgx.CopyFromSource) (int64, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Querier runs statements in the transaction InTx opened. Only InTx makes
// one, so a function that takes a Querier is guaranteed a transaction:
// SELECT … FOR UPDATE, SET LOCAL and pg_advisory_xact_lock belong in such
// functions (plan §3.5). A Querier has no Commit or Rollback.
//
// A Querier is valid only during its InTx callback. Keeping it afterwards
// is a bug: statements fail on the closed transaction and an AfterCommit
// hook registered on it never runs.
type Querier struct {
	statements
	tx    pgx.Tx
	hooks *[]func(context.Context)
}

// PgxTx returns the transaction for libraries that take a pgx.Tx, such as
// River's InsertTx: a job inserted through it commits or rolls back with
// the data (ADR-0010). Commit and Rollback on the returned value fail with
// ErrTxManaged; savepoints (Begin) work.
//
// This guards against mistakes, not against hostile code: the transaction
// can still be ended with a raw COMMIT statement.
func (q Querier) PgxTx() pgx.Tx { return managedTx{q.tx} }

// AfterCommit registers fn to run once the transaction has committed. It
// is for best-effort side effects such as a realtime broadcast (plan §3.5);
// a side effect that must not be lost is a River job inserted with PgxTx.
// fn does not run when the transaction rolls back or the commit fails. It
// gets a context that is not canceled with the request. A panic in fn is
// logged and the remaining hooks still run.
func (q Querier) AfterCommit(fn func(ctx context.Context)) {
	*q.hooks = append(*q.hooks, fn)
}

// InTx runs fn in a transaction on a single connection. It commits when fn
// returns nil and rolls back when fn returns an error or panics. fn's error
// is returned unchanged, so an *apperr.Error reaches the HTTP layer intact.
// Only if the rollback itself then fails (not merely because the
// connection was already closed) is the result errors.Join(fnErr,
// rollbackErr): errors.Is and errors.As still find fn's error, and the
// message carries both.
// After a successful commit the AfterCommit hooks run in order.
//
// fn receives a context marked as inside the transaction; InTx called
// with it returns ErrNestedTx. Do not make external calls (Iyzico, FCM,
// SMTP, S3) inside fn; commit first (CLAUDE.md, plan §3.5).
//
// A commit error is ambiguous. If the connection broke after COMMIT was
// sent, the transaction may have committed. Callers whose retry would not
// be safe either make the write idempotent (a unique key, River UniqueOpts)
// or read the outcome back before retrying.
func (d *DB) InTx(ctx context.Context, fn func(ctx context.Context, q Querier) error) error {
	if ctx.Value(txKey{}) != nil {
		return ErrNestedTx
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	var hooks []func(context.Context)
	q := Querier{statements: tx, tx: tx, hooks: &hooks}

	// The rollback must reach the server even when ctx is already canceled.
	cleanupCtx := context.WithoutCancel(ctx)
	committed := false
	defer func() {
		if !committed {
			// Covers a panic in fn. After a successful rollback or commit
			// this is a no-op.
			_ = tx.Rollback(cleanupCtx)
		}
	}()

	if err := fn(context.WithValue(ctx, txKey{}, struct{}{}), q); err != nil {
		if rbErr := tx.Rollback(cleanupCtx); rbErr != nil && !rollbackNoise(tx, rbErr) {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction (outcome unknown if the connection broke): %w", err)
	}
	committed = true

	for i, hook := range hooks {
		d.runHook(cleanupCtx, i, hook)
	}
	return nil
}

// rollbackNoise reports rollback errors that add nothing to fn's error:
// the transaction was already closed, or a canceled context made pgx close
// the connection, which ends the transaction on the server anyway.
func rollbackNoise(tx pgx.Tx, err error) bool {
	return errors.Is(err, pgx.ErrTxClosed) || tx.Conn().IsClosed()
}

// runHook runs one AfterCommit hook and contains its panic: the data is
// committed, so a failing best-effort side effect must neither crash the
// request nor skip the hooks after it.
func (d *DB) runHook(ctx context.Context, index int, hook func(context.Context)) {
	defer func() {
		if p := recover(); p != nil {
			d.logger.ErrorContext(ctx, "after-commit hook panicked",
				slog.Int("hook", index), slog.Any("panic", p))
		}
	}()
	hook(ctx)
}

// managedTx is a pgx.Tx whose end belongs to InTx.
type managedTx struct {
	pgx.Tx
}

func (managedTx) Commit(context.Context) error   { return ErrTxManaged }
func (managedTx) Rollback(context.Context) error { return ErrTxManaged }
