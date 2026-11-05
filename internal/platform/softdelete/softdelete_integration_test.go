//go:build integration

package softdelete_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// env is one test database with the application and maintenance pools and
// a Deleter over the merged catalog. The example tables
// (dbtest/testdata/migrations/9003_*):
//
//	example_owners <- example_projects (cascade) <- example_tasks (cascade)
//	                                             <- example_invoices (restrict)
//	               <- example_comments (detach)
type env struct {
	app, maint *db.DB
	del        *softdelete.Deleter
}

func newEnv(t *testing.T) env {
	t.Helper()
	d := dbtest.New(t)
	return env{
		app:   d.Open(t, db.RoleApp),
		maint: d.Open(t, db.RoleMaint),
		del:   softdelete.New(dbtest.SoftDeleteCatalog(t)),
	}
}

// Times of the application clock; restores happen after deletions.
var (
	deleteTime  = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	restoreTime = deleteTime.Add(time.Hour)
)

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// insert writes one row through an active view as koala_app.
func (e env) insert(t *testing.T, sql string, args ...any) uuid.UUID {
	t.Helper()
	id := newID()
	if _, err := e.app.Pool().Exec(t.Context(), sql, append([]any{id}, args...)...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (e env) owner(t *testing.T) uuid.UUID {
	t.Helper()
	return e.insert(t, "INSERT INTO active_example_owners (id, name) VALUES ($1, 'owner')")
}

func (e env) project(t *testing.T, owner uuid.UUID) uuid.UUID {
	t.Helper()
	return e.insert(t, "INSERT INTO active_example_projects (id, owner_id, name) VALUES ($1, $2, 'project')", owner)
}

func (e env) task(t *testing.T, project uuid.UUID, name string) uuid.UUID {
	t.Helper()
	return e.insert(t, "INSERT INTO active_example_tasks (id, project_id, name) VALUES ($1, $2, $3)", project, name)
}

func (e env) invoice(t *testing.T, project uuid.UUID) uuid.UUID {
	t.Helper()
	return e.insert(t, "INSERT INTO active_example_invoices (id, project_id, amount) VALUES ($1, $2, 15000)", project)
}

func (e env) comment(t *testing.T, author uuid.UUID) uuid.UUID {
	t.Helper()
	return e.insert(t, "INSERT INTO active_example_comments (id, author_id, body) VALUES ($1, $2, 'hi')", author)
}

// delete runs Delete in its own transaction as koala_app.
func (e env) delete(t *testing.T, table string, id uuid.UUID, actor softdelete.Actor) (softdelete.Deleted, error) {
	t.Helper()
	var out softdelete.Deleted
	err := e.app.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		var err error
		out, err = e.del.Delete(ctx, q, softdelete.DeleteRequest{
			Table: table, ID: id, Actor: actor, Reason: softdelete.ReasonUserRequest, At: deleteTime,
		})
		return err
	})
	return out, err
}

func (e env) mustDelete(t *testing.T, table string, id uuid.UUID) softdelete.Deleted {
	t.Helper()
	out, err := e.delete(t, table, id, softdelete.Actor{Kind: softdelete.ActorSystem})
	if err != nil {
		t.Fatalf("delete %s %s: %v", table, id, err)
	}
	return out
}

// restore runs Restore in its own transaction as koala_maint.
func (e env) restore(t *testing.T, deletionID uuid.UUID) (softdelete.Restored, error) {
	t.Helper()
	var out softdelete.Restored
	err := e.maint.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		var err error
		out, err = e.del.Restore(ctx, q, deletionID, restoreTime)
		return err
	})
	return out, err
}

// active reports whether the row is in the table's view, as koala_app sees
// it.
func (e env) active(t *testing.T, table string, id uuid.UUID) bool {
	t.Helper()
	var ok bool
	sql := "SELECT EXISTS (SELECT FROM active_" + table + " WHERE id = $1)"
	if err := e.app.Pool().QueryRow(t.Context(), sql, id).Scan(&ok); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return ok
}

// deletionOf returns the row's deletion_id from the base table.
func (e env) deletionOf(t *testing.T, table string, id uuid.UUID) uuid.NullUUID {
	t.Helper()
	var got uuid.NullUUID
	sql := "SELECT deletion_id FROM " + table + " WHERE id = $1"
	if err := e.maint.Pool().QueryRow(t.Context(), sql, id).Scan(&got); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return got
}

func (e env) noOrphans(t *testing.T) {
	t.Helper()
	err := e.maint.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		orphans, err := e.del.Orphans(ctx, q)
		if err != nil {
			return err
		}
		for _, o := range orphans {
			t.Errorf("orphans on %s: %d", o.Edge, o.Count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code apperr.Code) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != code || ae.Kind != apperr.KindConflict {
		t.Fatalf("err = %v, want 409 %s", err, code)
	}
}

func sortedIDs(ids ...uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return out
}

func wantRows(t *testing.T, what string, got, want map[string][]uuid.UUID) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: tables %v, want %v", what, got, want)
	}
	for table, ids := range want {
		if g := sortedIDs(got[table]...); !slices.Equal(g, sortedIDs(ids...)) {
			t.Errorf("%s: %s = %v, want %v", what, table, g, ids)
		}
	}
}

// Deleting an owner takes its projects and their tasks under one
// deletion_id, leaves the detached comment visible and records actor,
// reason and root. A task deleted earlier keeps its own deletion.
func TestDeleteCascades(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	owner := e.owner(t)
	p1, p2 := e.project(t, owner), e.project(t, owner)
	t1, t2 := e.task(t, p1, "a"), e.task(t, p1, "b")
	earlier := e.task(t, p2, "c")
	comment := e.comment(t, owner)
	other := e.owner(t)
	otherProject := e.project(t, other)

	before := e.mustDelete(t, "example_tasks", earlier)
	actor := softdelete.Actor{Kind: softdelete.ActorUser, ID: newID()}
	got, err := e.delete(t, "example_owners", owner, actor)
	if err != nil {
		t.Fatal(err)
	}

	wantRows(t, "deleted", got.Rows, map[string][]uuid.UUID{
		"example_owners":   {owner},
		"example_projects": {p1, p2},
		"example_tasks":    {t1, t2},
	})
	for table, ids := range got.Rows {
		for _, id := range ids {
			if e.active(t, table, id) {
				t.Errorf("%s %s still active", table, id)
			}
			if d := e.deletionOf(t, table, id); !d.Valid || d.UUID != got.DeletionID {
				t.Errorf("%s %s deletion_id = %v, want %s", table, id, d, got.DeletionID)
			}
		}
	}
	if d := e.deletionOf(t, "example_tasks", earlier); d.UUID != before.DeletionID {
		t.Errorf("earlier task's deletion_id = %v, want its own %s", d, before.DeletionID)
	}
	if !e.active(t, "example_comments", comment) {
		t.Error("detached comment disappeared with its author")
	}
	if !e.active(t, "example_owners", other) || !e.active(t, "example_projects", otherProject) {
		t.Error("another owner's rows were deleted")
	}

	var rootTable, actorKind, reason string
	var rootID uuid.UUID
	var actorID uuid.NullUUID
	var createdAt time.Time
	err = e.maint.Pool().QueryRow(t.Context(),
		"SELECT root_table, root_id, actor_kind, actor_id, reason, created_at FROM deletions WHERE id = $1",
		got.DeletionID).Scan(&rootTable, &rootID, &actorKind, &actorID, &reason, &createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if rootTable != "example_owners" || rootID != owner || actorKind != "user" ||
		actorID.UUID != actor.ID || reason != "user_request" || !createdAt.Equal(deleteTime) {
		t.Errorf("deletions row = %s %s %s %v %s %s", rootTable, rootID, actorKind, actorID, reason, createdAt)
	}
	e.noOrphans(t)
}

// Restore reopens exactly the rows of its deletion and stamps it; the task
// deleted on its own stays deleted.
func TestRestoreReopensOnlyItsDeletion(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	owner := e.owner(t)
	project := e.project(t, owner)
	task := e.task(t, project, "a")
	alone := e.task(t, project, "b")
	before := e.mustDelete(t, "example_tasks", alone)
	deleted := e.mustDelete(t, "example_owners", owner)

	got, err := e.restore(t, deleted.DeletionID)
	if err != nil {
		t.Fatal(err)
	}
	wantRows(t, "restored", got.Rows, map[string][]uuid.UUID{
		"example_owners":   {owner},
		"example_projects": {project},
		"example_tasks":    {task},
	})
	for _, r := range []struct {
		table string
		id    uuid.UUID
	}{{"example_owners", owner}, {"example_projects", project}, {"example_tasks", task}} {
		if !e.active(t, r.table, r.id) {
			t.Errorf("%s %s not restored", r.table, r.id)
		}
	}
	if e.active(t, "example_tasks", alone) {
		t.Error("a task deleted by another deletion was restored")
	}
	if d := e.deletionOf(t, "example_tasks", alone); d.UUID != before.DeletionID {
		t.Errorf("other deletion's task deletion_id = %v, want %s", d, before.DeletionID)
	}

	var restoredAt *time.Time
	if err := e.maint.Pool().QueryRow(t.Context(), "SELECT restored_at FROM deletions WHERE id = $1",
		deleted.DeletionID).Scan(&restoredAt); err != nil {
		t.Fatal(err)
	}
	if restoredAt == nil || !restoredAt.Equal(restoreTime) {
		t.Errorf("restored_at = %v, want %s", restoredAt, restoreTime)
	}
	e.noOrphans(t)

	if _, err := e.restore(t, deleted.DeletionID); !errors.Is(err, softdelete.ErrAlreadyRestored) {
		t.Errorf("second restore: err = %v, want ErrAlreadyRestored", err)
	}
	if _, err := e.restore(t, newID()); !errors.Is(err, softdelete.ErrDeletionNotFound) {
		t.Errorf("unknown deletion: err = %v, want ErrDeletionNotFound", err)
	}
}

// A restrict edge with an active child stops the whole deletion, also when
// the cascade reaches it one level down; nothing is left behind.
func TestRestrictStopsTheDeletion(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	owner := e.owner(t)
	project := e.project(t, owner)
	invoice := e.invoice(t, project)

	_, err := e.delete(t, "example_owners", owner, softdelete.Actor{Kind: softdelete.ActorSystem})
	wantCode(t, err, softdelete.CodeRestricted)
	if !e.active(t, "example_owners", owner) || !e.active(t, "example_projects", project) {
		t.Error("a restricted deletion left rows deleted")
	}
	var n int
	if err := e.maint.Pool().QueryRow(t.Context(), "SELECT count(*) FROM deletions WHERE root_id = $1", owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a restricted deletion left %d deletions rows", n)
	}

	e.mustDelete(t, "example_invoices", invoice)
	if _, err := e.delete(t, "example_owners", owner, softdelete.Actor{Kind: softdelete.ActorSystem}); err != nil {
		t.Fatalf("delete after the invoice is gone: %v", err)
	}
}

func TestDeleteNeedsAnActiveRoot(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	system := softdelete.Actor{Kind: softdelete.ActorSystem}
	if _, err := e.delete(t, "example_owners", newID(), system); !errors.Is(err, softdelete.ErrNotActive) {
		t.Errorf("missing root: err = %v, want ErrNotActive", err)
	}
	owner := e.owner(t)
	e.mustDelete(t, "example_owners", owner)
	if _, err := e.delete(t, "example_owners", owner, system); !errors.Is(err, softdelete.ErrNotActive) {
		t.Errorf("deleted root: err = %v, want ErrNotActive", err)
	}
}

// A restore whose row's unique key was taken meanwhile is a 409 and
// changes nothing.
func TestRestoreConflict(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	project := e.project(t, e.owner(t))
	task := e.task(t, project, "same name")
	deleted := e.mustDelete(t, "example_tasks", task)
	e.task(t, project, "same name")

	_, err := e.restore(t, deleted.DeletionID)
	wantCode(t, err, softdelete.CodeRestoreConflict)
	if e.active(t, "example_tasks", task) {
		t.Error("conflicting restore reopened the task")
	}
}

// A row whose parent was deleted by another deletion cannot come back on
// its own.
func TestRestoreParentDeleted(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	project := e.project(t, e.owner(t))
	task := e.task(t, project, "a")
	taskDeletion := e.mustDelete(t, "example_tasks", task)
	e.mustDelete(t, "example_projects", project)

	_, err := e.restore(t, taskDeletion.DeletionID)
	wantCode(t, err, softdelete.CodeRestoreParentDeleted)
	if e.active(t, "example_tasks", task) {
		t.Error("task restored under a deleted project")
	}
	e.noOrphans(t)
}

// Restore works on base tables: as koala_app it is permission denied.
func TestRestoreNeedsMaint(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	deleted := e.mustDelete(t, "example_owners", e.owner(t))
	err := e.app.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		_, err := e.del.Restore(ctx, q, deleted.DeletionID, restoreTime)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("restore as koala_app: err = %v, want permission denied", err)
	}
}

// Orphans finds an active child under a deleted parent: the invariant the
// monitoring job watches.
func TestOrphansFindsBrokenCascade(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	project := e.project(t, e.owner(t))
	task := e.task(t, project, "a")
	e.mustDelete(t, "example_projects", project)
	// Break the invariant by hand, as a buggy maintenance query would.
	if _, err := e.maint.Pool().Exec(t.Context(),
		"UPDATE example_tasks SET deleted_at = NULL, deletion_id = NULL WHERE id = $1", task); err != nil {
		t.Fatal(err)
	}
	err := e.maint.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		orphans, err := e.del.Orphans(ctx, q)
		if err != nil {
			return err
		}
		if len(orphans) != 1 || orphans[0].Edge.Child != "example_tasks" || orphans[0].Count != 1 {
			t.Errorf("orphans = %v, want one on example_tasks.project_id", orphans)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
