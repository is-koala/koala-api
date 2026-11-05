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
package softdelete

import (
	"context"
	"errors"
	"fmt"
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
	// CodeRestoreConflict: a restored row's unique key is taken by an
	// active row (409).
	CodeRestoreConflict apperr.Code = "RESTORE_CONFLICT"
	// CodeRestoreParentDeleted: a restored row's parent on a cascade or
	// restrict edge is deleted by another deletion (409).
	CodeRestoreParentDeleted apperr.Code = "RESTORE_PARENT_DELETED"
)

// Codes returns the error codes this package returns to clients.
func Codes() []apperr.Code {
	return []apperr.Code{CodeRestricted, CodeRestoreConflict, CodeRestoreParentDeleted}
}

// ErrNotActive means the root of a Delete does not exist or is already
// deleted. The caller answers with its resource's own 404.
var ErrNotActive = errors.New("softdelete: root row is not active")

// ErrDeletionNotFound means Restore got an unknown deletion id.
var ErrDeletionNotFound = errors.New("softdelete: deletion not found")

// ErrAlreadyRestored means Restore got a deletion that was restored before.
var ErrAlreadyRestored = errors.New("softdelete: deletion already restored")

// ActorKind is who deleted: a user, a staff member or the system.
type ActorKind string

// Actor kinds (deletions_actor_kind_check).
const (
	ActorUser   ActorKind = "user"
	ActorStaff  ActorKind = "staff"
	ActorSystem ActorKind = "system"
)

// Actor is who deleted. ID is the user's or staff member's id and zero for
// the system.
type Actor struct {
	Kind ActorKind
	ID   uuid.UUID
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
// q's transaction, as koala_app (through the active views). A restrict edge
// with an active child fails the call with 409 SOFT_DELETE_RESTRICTED;
// the caller's InTx rolls everything back. Detach edges are left alone.
// A root that is missing or already deleted gives ErrNotActive.
func (d *Deleter) Delete(ctx context.Context, q db.Querier, r DeleteRequest) (Deleted, error) {
	root, ok := d.cat.Table(r.Table)
	if !ok || !root.SoftDelete() {
		return Deleted{}, fmt.Errorf("softdelete: %q is not a soft-deleted table of the catalog", r.Table)
	}
	if err := r.validate(); err != nil {
		return Deleted{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Deleted{}, fmt.Errorf("softdelete: new deletion id: %w", err)
	}
	actorID := uuid.NullUUID{UUID: r.Actor.ID, Valid: r.Actor.Kind != ActorSystem}
	// The deletions row comes first: deletion_id references it.
	err = softdeletedb.New(q).InsertDeletion(ctx, softdeletedb.InsertDeletionParams{
		ID: id, RootTable: r.Table, RootID: r.ID,
		ActorKind: string(r.Actor.Kind), ActorID: actorID,
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
			switch e.Policy {
			case Restrict:
				if err := d.checkRestrict(ctx, q, e, b.ids); err != nil {
					return Deleted{}, err
				}
			case Cascade:
				// Only active rows are updated, so a cycle (a tree's
				// self-reference) ends when nothing is left to mark.
				children, err := d.mark(ctx, q, e.Child, e.Column, b.ids, id, r.At)
				if err != nil {
					return Deleted{}, err
				}
				if len(children) > 0 {
					queue = append(queue, batch{e.Child, children})
				}
			case Detach:
			}
		}
	}
	return out, nil
}

func (r DeleteRequest) validate() error {
	switch r.Actor.Kind {
	case ActorUser, ActorStaff:
		if r.Actor.ID == uuid.Nil {
			return fmt.Errorf("softdelete: a %s actor needs an id", r.Actor.Kind)
		}
	case ActorSystem:
		if r.Actor.ID != uuid.Nil {
			return errors.New("softdelete: the system actor has no id")
		}
	default:
		return fmt.Errorf("softdelete: unknown actor kind %q", r.Actor.Kind)
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
// one of parents.
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

// Restore reopens the rows of one deletion, in q's transaction, as
// koala_maint (on the base tables; koala_app gets permission denied). It
// fails with 409 RESTORE_CONFLICT when a reopened row's unique key is taken,
// and with 409 RESTORE_PARENT_DELETED when a reopened row's parent on a
// cascade or restrict edge is still deleted; the caller's InTx rolls back.
// The deletions row stays and gets restored_at = at.
func (d *Deleter) Restore(ctx context.Context, q db.Querier, deletionID uuid.UUID, at time.Time) (Restored, error) {
	mq := maintdb.New(q)
	del, err := mq.LockDeletion(ctx, deletionID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Restored{}, fmt.Errorf("%w: %s", ErrDeletionNotFound, deletionID)
	case err != nil:
		return Restored{}, fmt.Errorf("softdelete: load deletion %s: %w", deletionID, err)
	case del.RestoredAt != nil:
		return Restored{}, fmt.Errorf("%w: %s", ErrAlreadyRestored, deletionID)
	}

	out := Restored{Rows: map[string][]uuid.UUID{}}
	for _, table := range d.cat.SoftDeleteTables() {
		sql := "UPDATE " + ident(table) +
			" SET deleted_at = NULL, deletion_id = NULL WHERE deletion_id = $1 RETURNING id"
		rows, err := q.Query(ctx, sql, deletionID)
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

	for _, e := range d.cat.Edges() {
		ids := out.Rows[e.Child]
		if e.Policy == Detach || len(ids) == 0 {
			continue
		}
		sql := "SELECT EXISTS (SELECT FROM " + ident(e.Child) + " c JOIN " + ident(e.Parent) +
			" p ON p.id = c." + ident(e.Column) + " WHERE c.id = ANY($1) AND p.deleted_at IS NOT NULL)"
		var orphaned bool
		if err := q.QueryRow(ctx, sql, ids).Scan(&orphaned); err != nil {
			return Restored{}, fmt.Errorf("softdelete: check parents on %s: %w", e, err)
		}
		if orphaned {
			return Restored{}, apperr.Wrap(fmt.Errorf("parent deleted on %s", e), apperr.KindConflict, CodeRestoreParentDeleted)
		}
	}

	if err := mq.MarkDeletionRestored(ctx, maintdb.MarkDeletionRestoredParams{ID: deletionID, RestoredAt: &at}); err != nil {
		return Restored{}, fmt.Errorf("softdelete: mark deletion %s restored: %w", deletionID, err)
	}
	return out, nil
}

// uniqueViolation is SQLSTATE 23505.
const uniqueViolation = "23505"

func restoreError(table string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
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
