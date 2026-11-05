// Package softdelete is the soft-delete platform of ADR-0013 and ADR-0020.
//
// A soft-deleted table has deleted_at and deletion_id columns and an
// active_<t> view (WHERE deleted_at IS NULL). koala_app reads and writes the
// views only; the base tables belong to koala_maint. db/softdelete.yaml (the
// Catalog) classifies every table and gives every foreign key to a
// soft-deleted table a policy: cascade, restrict or detach.
//
// Deletion goes through Deleter.Delete alone: it writes one deletions row,
// marks the root and follows the catalog's cascade edges in the caller's
// transaction, all under the same deletion_id. Deleter.Restore reopens
// exactly the rows of one deletion, as koala_maint. Cascades are not database
// triggers: the catalog is the one place that says what a deletion takes
// with it.
//
// # Concurrency
//
// The invariant is "no active child under a deleted parent" on cascade and
// restrict edges (Orphans). It holds under READ COMMITTED because every
// path that makes a child active takes a lock on its parent row that
// conflicts with the row lock of a deleting UPDATE:
//
//   - Delete marks parents (UPDATE, a row lock) before it cascades and
//     before it checks restrict edges, each in a new statement that sees
//     what committed meanwhile.
//   - Restore locks the parents of every reopened row FOR SHARE, without
//     filtering on deleted_at, and checks deleted_at in Go: it waits for a
//     concurrent Delete and then sees its result, or holds the parent so
//     that the Delete waits and then sees the reopened child.
//   - Code that inserts a child, or points a child at another parent,
//     first calls LockActiveParent in the same transaction (CLAUDE.md).
//
// Two deletions of overlapping subtrees can deadlock (SQLSTATE 40P01);
// Postgres aborts one of them. Deletion is not on a hot path: the caller
// reports the error and the user or job retries. InTx does not retry
// because it cannot know whether its callback is safe to run twice.
//
// Delete runs in one transaction under koala_app's 20 s statement timeout.
// A subtree of many thousands of rows (a company with its whole history)
// is deleted by a River job as the system actor, not inside a request.
package softdelete

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/db/maintdb"
	"github.com/is-koala/koala-api/internal/platform/softdelete/softdeletedb"
)

// Error codes of this package (api/components/problem.yaml ErrorCode).
const (
	// CodeRestricted: a restrict edge has an active child (409).
	CodeRestricted apperr.Code = "SOFT_DELETE_RESTRICTED"
	// CodeRestoreConflict: a restored row's unique key (or exclusion
	// constraint) is taken by an active row (409).
	CodeRestoreConflict apperr.Code = "RESTORE_CONFLICT"
	// CodeRestoreParentDeleted: a restored row's parent on a cascade or
	// restrict edge is deleted by another deletion (409).
	CodeRestoreParentDeleted apperr.Code = "RESTORE_PARENT_DELETED"
	// CodeRestoreExpired: the deletion's root row no longer exists to be
	// reopened, for example after a purge (409).
	CodeRestoreExpired apperr.Code = "RESTORE_EXPIRED"
	// CodeDeletionNotFound: Restore got an unknown deletion id (404).
	CodeDeletionNotFound apperr.Code = "DELETION_NOT_FOUND"
	// CodeDeletionAlreadyRestored: Restore got a deletion that was
	// restored before (409).
	CodeDeletionAlreadyRestored apperr.Code = "DELETION_ALREADY_RESTORED"
)

// Codes returns the error codes this package returns to clients.
func Codes() []apperr.Code {
	return []apperr.Code{
		CodeRestricted, CodeRestoreConflict, CodeRestoreParentDeleted,
		CodeRestoreExpired, CodeDeletionNotFound, CodeDeletionAlreadyRestored,
	}
}

// ErrNotActive means the root of a Delete, or the parent of
// LockActiveParent, does not exist or is deleted. The caller answers with
// its resource's own 404 (or 409 for a parent).
//
// Delete has already written its deletions row in q's transaction when it
// returns ErrNotActive: the caller must return the error from its InTx
// callback so that the transaction rolls back, never commit after it.
var ErrNotActive = errors.New("softdelete: row is not active")

// ActorKind is who deleted or restored: a user, a staff member or the
// system.
type ActorKind string

// Actor kinds (deletions_actor_kind_check, deletions_restored_by_kind_check).
const (
	ActorUser   ActorKind = "user"
	ActorStaff  ActorKind = "staff"
	ActorSystem ActorKind = "system"
)

// Actor is who deleted or restored. ID is the user's or staff member's id
// and zero for the system.
type Actor struct {
	Kind ActorKind
	ID   uuid.UUID
}

func (a Actor) validate() error {
	switch a.Kind {
	case ActorUser, ActorStaff:
		if a.ID == uuid.Nil {
			return fmt.Errorf("softdelete: a %s actor needs an id", a.Kind)
		}
	case ActorSystem:
		if a.ID != uuid.Nil {
			return errors.New("softdelete: the system actor has no id")
		}
	default:
		return fmt.Errorf("softdelete: unknown actor kind %q", a.Kind)
	}
	return nil
}

func (a Actor) nullID() uuid.NullUUID {
	return uuid.NullUUID{UUID: a.ID, Valid: a.Kind != ActorSystem}
}

// Reason is why a row was deleted (deletions_reason_check).
type Reason string

// Reasons.
const (
	ReasonUserRequest     Reason = "user_request"
	ReasonStaffAction     Reason = "staff_action"
	ReasonRetention       Reason = "retention"
	ReasonAccountDeletion Reason = "account_deletion"
)

// DeleteRequest is one soft deletion.
type DeleteRequest struct {
	// Table is the soft-deleted table of the root, ID its id.
	Table string
	ID    uuid.UUID
	Actor Actor
	// Reason is why.
	Reason Reason
	// At is the application clock's time; it becomes deleted_at.
	At time.Time
}

func (r DeleteRequest) validate() error {
	if err := r.Actor.validate(); err != nil {
		return err
	}
	switch r.Reason {
	case ReasonUserRequest, ReasonStaffAction, ReasonRetention, ReasonAccountDeletion:
	default:
		return fmt.Errorf("softdelete: unknown reason %q", r.Reason)
	}
	if r.At.IsZero() {
		return errors.New("softdelete: At is required (the application clock's time)")
	}
	return nil
}

// RestoreRequest is the restore of one deletion.
type RestoreRequest struct {
	DeletionID uuid.UUID
	// Actor is who restores; it is stamped on the deletions row. The audit
	// record of a staff restore comes with the audit core (Faz 8).
	Actor Actor
	// At is the application clock's time; it becomes restored_at.
	At time.Time
}

// Deleted is the outcome of Delete.
type Deleted struct {
	// DeletionID groups the rows; Restore takes it.
	DeletionID uuid.UUID
	// Rows lists the ids marked per table, the root's table included.
	Rows map[string][]uuid.UUID
}

// Restored is the outcome of Restore.
type Restored struct {
	// Rows lists the ids reopened per table.
	Rows map[string][]uuid.UUID
}

// Deleter deletes and restores along a catalog.
type Deleter struct {
	cat *Catalog
}

// New returns a Deleter for cat.
func New(cat *Catalog) *Deleter {
	return &Deleter{cat: cat}
}

// Delete soft-deletes r's root and everything its cascade edges reach, in
// q's transaction, as koala_app (through the active views). After the
// cascade it checks every restrict edge against all rows it marked, so the
// outcome does not depend on the order of the edges: a child that the same
// deletion takes along does not block it, any other active child fails the
// call with 409 SOFT_DELETE_RESTRICTED. Detach edges are left alone. A root
// that is missing or already deleted gives ErrNotActive. On any error the
// caller's InTx must roll back (see ErrNotActive).
func (d *Deleter) Delete(ctx context.Context, q db.Querier, r DeleteRequest) (Deleted, error) {
	root, ok := d.cat.Table(r.Table)
	if !ok || !root.SoftDelete() {
		return Deleted{}, fmt.Errorf("softdelete: %q is not a soft-deleted table of the catalog", r.Table)
	}
	if err := r.validate(); err != nil {
		return Deleted{}, err
	}
	// The application makes the id: koala_app may INSERT into deletions but
	// not read it back.
	id, err := uuid.NewV7()
	if err != nil {
		return Deleted{}, fmt.Errorf("softdelete: new deletion id: %w", err)
	}
	// The deletions row comes first: deletion_id references it.
	err = softdeletedb.New(q).InsertDeletion(ctx, softdeletedb.InsertDeletionParams{
		ID: id, RootTable: r.Table, RootID: r.ID,
		ActorKind: string(r.Actor.Kind), ActorID: r.Actor.nullID(),
		Reason: string(r.Reason), CreatedAt: r.At,
	})
	if err != nil {
		return Deleted{}, fmt.Errorf("softdelete: record deletion of %s %s: %w", r.Table, r.ID, err)
	}

	out := Deleted{DeletionID: id, Rows: map[string][]uuid.UUID{}}
	marked, err := d.mark(ctx, q, r.Table, "id", []uuid.UUID{r.ID}, id, r.At)
	if err != nil {
		return Deleted{}, err
	}
	if len(marked) == 0 {
		return Deleted{}, fmt.Errorf("%w: %s %s", ErrNotActive, r.Table, r.ID)
	}

	type batch struct {
		table string
		ids   []uuid.UUID
	}
	queue := []batch{{r.Table, marked}}
	for len(queue) > 0 {
		b := queue[0]
		queue = queue[1:]
		out.Rows[b.table] = append(out.Rows[b.table], b.ids...)
		for _, e := range d.cat.edgesFrom(b.table) {
			if e.Policy != Cascade {
				continue
			}
			// Only active rows are updated, so a cycle (a tree's
			// self-reference) ends when nothing is left to mark.
			children, err := d.mark(ctx, q, e.Child, e.Column, b.ids, id, r.At)
			if err != nil {
				return Deleted{}, err
			}
			if len(children) > 0 {
				queue = append(queue, batch{e.Child, children})
			}
		}
	}

	for _, e := range d.cat.Edges() {
		if parents := out.Rows[e.Parent]; e.Policy == Restrict && len(parents) > 0 {
			if err := d.checkRestrict(ctx, q, e, parents); err != nil {
				return Deleted{}, err
			}
		}
	}
	return out, nil
}

// mark soft-deletes the active rows of table whose column is in ids, through
// the table's view, and returns their ids.
func (d *Deleter) mark(ctx context.Context, q db.Querier, table, column string, ids []uuid.UUID, deletionID uuid.UUID, at time.Time) ([]uuid.UUID, error) {
	// Names come from the validated catalog and are quoted.
	sql := "UPDATE " + ident(d.cat.relation(table)) +
		" SET deleted_at = $1, deletion_id = $2 WHERE " + ident(column) + " = ANY($3) RETURNING id"
	rows, err := q.Query(ctx, sql, at, deletionID, ids)
	if err != nil {
		return nil, fmt.Errorf("softdelete: mark %s: %w", table, err)
	}
	marked, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("softdelete: mark %s: %w", table, err)
	}
	return marked, nil
}

// checkRestrict fails when e's child table has an active row pointing at
// one of parents. The parents are already marked (row-locked) by this
// transaction, so a child added concurrently under LockActiveParent either
// committed before this statement (and is seen) or waits and then finds the
// parent deleted.
func (d *Deleter) checkRestrict(ctx context.Context, q db.Querier, e Edge, parents []uuid.UUID) error {
	sql := "SELECT EXISTS (SELECT FROM " + ident(d.cat.relation(e.Child)) +
		" WHERE " + ident(e.Column) + " = ANY($1))"
	var found bool
	if err := q.QueryRow(ctx, sql, parents).Scan(&found); err != nil {
		return fmt.Errorf("softdelete: check %s: %w", e, err)
	}
	if found {
		return apperr.Wrap(fmt.Errorf("active rows in %s", e), apperr.KindConflict, CodeRestricted)
	}
	return nil
}

// LockActiveParent locks the active row id of the soft-deleted table FOR
// SHARE, through its view, in q's transaction. Code that inserts a child
// row, or points a child at another parent, calls it first: a concurrent
// Delete of the parent then either waits for this transaction and cascades
// to (or is restricted by) the new child, or has already marked the parent
// and this call returns ErrNotActive.
func LockActiveParent(ctx context.Context, q db.Querier, table string, id uuid.UUID) error {
	if !identifier.MatchString(table) {
		return fmt.Errorf("softdelete: invalid table name %q", table)
	}
	// Through the view: if a Delete marks the row first, the lock waits for
	// it and the re-checked view filter then drops the row.
	sql := "SELECT id FROM " + ident(ViewPrefix+table) + " WHERE id = $1 FOR SHARE"
	var locked uuid.UUID
	err := q.QueryRow(ctx, sql, id).Scan(&locked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: parent %s %s", ErrNotActive, table, id)
	case err != nil:
		return fmt.Errorf("softdelete: lock %s %s: %w", table, id, err)
	}
	return nil
}

// Restore reopens the rows of one deletion, in q's transaction, as
// koala_maint (on the base tables; koala_app gets permission denied). It
// fails with
//   - 404 DELETION_NOT_FOUND for an unknown deletion and 409
//     DELETION_ALREADY_RESTORED for one restored before;
//   - 409 RESTORE_CONFLICT when a reopened row's unique key is taken;
//   - 409 RESTORE_PARENT_DELETED when a reopened row's parent on a cascade
//     or restrict edge is deleted (also by a Delete that commits while
//     Restore waits for the parent's lock);
//   - 409 RESTORE_EXPIRED when the root row is gone.
//
// The caller's InTx rolls back on any error. The deletions row stays and
// gets restored_at and the restoring actor.
func (d *Deleter) Restore(ctx context.Context, q db.Querier, r RestoreRequest) (Restored, error) {
	if err := r.Actor.validate(); err != nil {
		return Restored{}, err
	}
	if r.At.IsZero() {
		return Restored{}, errors.New("softdelete: At is required (the application clock's time)")
	}
	mq := maintdb.New(q)
	del, err := mq.LockDeletion(ctx, r.DeletionID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Restored{}, apperr.Wrap(fmt.Errorf("deletion %s", r.DeletionID), apperr.KindNotFound, CodeDeletionNotFound)
	case err != nil:
		return Restored{}, fmt.Errorf("softdelete: load deletion %s: %w", r.DeletionID, err)
	case del.RestoredAt != nil:
		return Restored{}, apperr.Wrap(fmt.Errorf("deletion %s", r.DeletionID), apperr.KindConflict, CodeDeletionAlreadyRestored)
	}

	out := Restored{Rows: map[string][]uuid.UUID{}}
	for _, table := range d.cat.SoftDeleteTables() {
		sql := "UPDATE " + ident(table) +
			" SET deleted_at = NULL, deletion_id = NULL WHERE deletion_id = $1 RETURNING id"
		rows, err := q.Query(ctx, sql, r.DeletionID)
		if err != nil {
			return Restored{}, restoreError(table, err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return Restored{}, restoreError(table, err)
		}
		if len(ids) > 0 {
			out.Rows[table] = ids
		}
	}
	if !slices.Contains(out.Rows[del.RootTable], del.RootID) {
		return Restored{}, apperr.Wrap(fmt.Errorf("root %s %s of deletion %s is gone", del.RootTable, del.RootID, r.DeletionID),
			apperr.KindConflict, CodeRestoreExpired)
	}

	for _, e := range d.cat.Edges() {
		ids := out.Rows[e.Child]
		if e.Policy == Detach || len(ids) == 0 {
			continue
		}
		if err := d.checkParents(ctx, q, e, ids); err != nil {
			return Restored{}, err
		}
	}

	n, err := mq.MarkDeletionRestored(ctx, maintdb.MarkDeletionRestoredParams{
		ID: r.DeletionID, RestoredAt: r.At,
		RestoredByKind: string(r.Actor.Kind), RestoredBy: r.Actor.nullID(),
	})
	if err != nil {
		return Restored{}, fmt.Errorf("softdelete: mark deletion %s restored: %w", r.DeletionID, err)
	}
	if n != 1 {
		return Restored{}, fmt.Errorf("softdelete: mark deletion %s restored: %d rows", r.DeletionID, n)
	}
	return out, nil
}

// checkParents locks the parents of the reopened rows ids on edge e FOR
// SHARE and fails when one is deleted. The lock takes every parent, with no
// filter on deleted_at: a filtered lock would skip the parent that a
// concurrent Delete is about to mark. deleted_at is checked here, after the
// lock, on the row version the lock returns.
func (d *Deleter) checkParents(ctx context.Context, q db.Querier, e Edge, ids []uuid.UUID) error {
	sql := "SELECT p.deleted_at FROM " + ident(e.Child) + " c JOIN " + ident(e.Parent) +
		" p ON p.id = c." + ident(e.Column) + " WHERE c.id = ANY($1) FOR SHARE OF p"
	rows, err := q.Query(ctx, sql, ids)
	if err != nil {
		return fmt.Errorf("softdelete: lock parents on %s: %w", e, err)
	}
	deletedAt, err := pgx.CollectRows(rows, pgx.RowTo[*time.Time])
	if err != nil {
		return fmt.Errorf("softdelete: lock parents on %s: %w", e, err)
	}
	for _, at := range deletedAt {
		if at != nil {
			return apperr.Wrap(fmt.Errorf("parent deleted on %s", e), apperr.KindConflict, CodeRestoreParentDeleted)
		}
	}
	return nil
}

// SQLSTATEs a restore maps to RESTORE_CONFLICT.
const (
	uniqueViolation    = "23505"
	exclusionViolation = "23P01"
)

func restoreError(table string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == uniqueViolation || pgErr.Code == exclusionViolation) {
		return apperr.Wrap(fmt.Errorf("restore %s: %s: %w", table, pgErr.ConstraintName, err), apperr.KindConflict, CodeRestoreConflict)
	}
	return fmt.Errorf("softdelete: restore %s: %w", table, err)
}

// Orphan is a cascade or restrict edge with active children of deleted
// parents, a broken invariant (plan §3.6-3, "değişmez sorgusu").
type Orphan struct {
	Edge  Edge
	Count int64
}

// Orphans counts, per cascade and restrict edge, the active child rows whose
// parent is deleted; it returns the edges where the count is not zero. It
// reads base tables and runs as koala_maint, in tests and in monitoring.
func (d *Deleter) Orphans(ctx context.Context, q db.Querier) ([]Orphan, error) {
	var out []Orphan
	for _, e := range d.cat.Edges() {
		if e.Policy == Detach {
			continue
		}
		sql := "SELECT count(*) FROM " + ident(e.Child) + " c JOIN " + ident(e.Parent) +
			" p ON p.id = c." + ident(e.Column) + " WHERE p.deleted_at IS NOT NULL"
		if t, _ := d.cat.Table(e.Child); t.SoftDelete() {
			sql += " AND c.deleted_at IS NULL"
		}
		var n int64
		if err := q.QueryRow(ctx, sql).Scan(&n); err != nil {
			return nil, fmt.Errorf("softdelete: count orphans on %s: %w", e, err)
		}
		if n > 0 {
			out = append(out, Orphan{Edge: e, Count: n})
		}
	}
	return out, nil
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }
