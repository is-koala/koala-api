//go:build integration

package softdelete_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// env is one test database with the application, maintenance and owner
// pools and a Deleter over the merged catalog. The example tables
// (dbtest/testdata/migrations/9003_*):
//
//	example_owners <- example_projects (cascade) <- example_tasks (cascade) <- example_invoices.task_id (cascade)
//	                                             <- example_invoices.project_id (restrict)
//	                                             <- example_receipts (restrict, M1)
//	               <- example_comments (detach)
//	example_folders.parent_id -> example_folders (cascade)
type env struct {
	app, maint, owner *db.DB
	del               *softdelete.Deleter
	// restorer is the staff member every restore of the env is made by.
	restorer softdelete.Actor
}

func newEnv(t *testing.T) env {
	t.Helper()
	d := dbtest.New(t)
	return env{
		app:      d.Open(t, db.RoleApp),
		maint:    d.Open(t, db.RoleMaint),
		owner:    d.Open(t, db.RoleOwner),
		del:      softdelete.New(dbtest.SoftDeleteCatalog(t)),
		restorer: softdelete.Actor{Kind: softdelete.ActorStaff, ID: newID()},
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

func (e env) invoice(t *testing.T, project uuid.UUID, task uuid.NullUUID) uuid.UUID {
	t.Helper()
	return e.insert(t, "INSERT INTO active_example_invoices (id, project_id, task_id, amount) VALUES ($1, $2, $3, 15000)", project, task)
}

func (e env) folder(t *testing.T, parent uuid.NullUUID) uuid.UUID {
	t.Helper()
	return e.insert(t, "INSERT INTO active_example_folders (id, parent_id, name) VALUES ($1, $2, 'f')", parent)
}

func (e env) receipt(t *testing.T, project uuid.UUID) uuid.UUID {
	t.Helper()
	return e.insert(t, "INSERT INTO example_receipts (id, project_id, amount) VALUES ($1, $2, 15000)", project)
}

func some(id uuid.UUID) uuid.NullUUID { return uuid.NullUUID{UUID: id, Valid: true} }

func (e env) comment(t *testing.T, author uuid.UUID) uuid.UUID {
	t.Helper()
	return e.insert(t, "INSERT INTO active_example_comments (id, author_id, body) VALUES ($1, $2, 'hi')", author)
}

func deleteRequest(table string, id uuid.UUID, actor softdelete.Actor) softdelete.DeleteRequest {
	return softdelete.DeleteRequest{Table: table, ID: id, Actor: actor, Reason: softdelete.ReasonUserRequest, At: deleteTime}
}

var system = softdelete.Actor{Kind: softdelete.ActorSystem}

// delete runs Delete in its own transaction as koala_app.
func (e env) delete(t *testing.T, table string, id uuid.UUID, actor softdelete.Actor) (softdelete.Deleted, error) {
	t.Helper()
	var out softdelete.Deleted
	err := e.app.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		var err error
		out, err = e.del.Delete(ctx, q, deleteRequest(table, id, actor))
		return err
	})
	return out, err
}

func (e env) restoreRequest(deletionID uuid.UUID) softdelete.RestoreRequest {
	return softdelete.RestoreRequest{DeletionID: deletionID, Actor: e.restorer, At: restoreTime}
}

func (e env) mustDelete(t *testing.T, table string, id uuid.UUID) softdelete.Deleted {
	t.Helper()
	out, err := e.delete(t, table, id, system)
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
		out, err = e.del.Restore(ctx, q, e.restoreRequest(deletionID))
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

// wantCode fails unless err is a 409 with code.
func wantCode(t *testing.T, err error, code apperr.Code) {
	t.Helper()
	wantError(t, err, apperr.KindConflict, code)
}

func wantError(t *testing.T, err error, kind apperr.Kind, code apperr.Code) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != code || ae.Kind != kind {
		t.Fatalf("err = %v, want %s %s", err, kind, code)
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
	var restoredByKind string
	var restoredBy uuid.NullUUID
	if err := e.maint.Pool().QueryRow(t.Context(), "SELECT restored_at, restored_by_kind, restored_by FROM deletions WHERE id = $1",
		deleted.DeletionID).Scan(&restoredAt, &restoredByKind, &restoredBy); err != nil {
		t.Fatal(err)
	}
	if restoredAt == nil || !restoredAt.Equal(restoreTime) || restoredByKind != "staff" || restoredBy.UUID != e.restorer.ID {
		t.Errorf("restore stamp = %v %s %v, want %s staff %s", restoredAt, restoredByKind, restoredBy, restoreTime, e.restorer.ID)
	}
	e.noOrphans(t)

	_, err = e.restore(t, deleted.DeletionID)
	wantCode(t, err, softdelete.CodeDeletionAlreadyRestored)
	_, err = e.restore(t, newID())
	wantError(t, err, apperr.KindNotFound, softdelete.CodeDeletionNotFound)
}

// A restrict edge with an active child stops the whole deletion, also when
// the cascade reaches it one level down; nothing is left behind.
func TestRestrictStopsTheDeletion(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	owner := e.owner(t)
	project := e.project(t, owner)
	invoice := e.invoice(t, project, uuid.NullUUID{})

	_, err := e.delete(t, "example_owners", owner, system)
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
	if _, err := e.delete(t, "example_owners", owner, system); err != nil {
		t.Fatalf("delete after the invoice is gone: %v", err)
	}
}

func TestDeleteNeedsAnActiveRoot(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
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
		_, err := e.del.Restore(ctx, q, e.restoreRequest(deleted.DeletionID))
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

// The restrict check runs after the cascade, over every row the deletion
// marked: an invoice that the deletion itself takes along (through its
// task) does not block the project's restrict edge, whatever the order in
// which the edges are visited. An invoice the deletion does not reach still
// does.
func TestRestrictIgnoresChildrenOfTheSameDeletion(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	owner := e.owner(t)
	project := e.project(t, owner)
	task := e.task(t, project, "a")
	invoice := e.invoice(t, project, some(task))

	got, err := e.delete(t, "example_owners", owner, system)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	wantRows(t, "deleted", got.Rows, map[string][]uuid.UUID{
		"example_owners":   {owner},
		"example_projects": {project},
		"example_tasks":    {task},
		"example_invoices": {invoice},
	})
	e.noOrphans(t)

	other := e.owner(t)
	otherProject := e.project(t, other)
	e.task(t, otherProject, "a")
	loose := e.invoice(t, otherProject, uuid.NullUUID{})
	_, err = e.delete(t, "example_owners", other, system)
	wantCode(t, err, softdelete.CodeRestricted)
	if !e.active(t, "example_invoices", loose) || !e.active(t, "example_owners", other) {
		t.Error("a restricted deletion left rows deleted")
	}
}

// A tree (a self-referencing cascade edge) is deleted to the leaves, and a
// cycle in it ends because only active rows are marked.
func TestDeleteFollowsATreeAndACycle(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	root := e.folder(t, uuid.NullUUID{})
	child := e.folder(t, some(root))
	leaf := e.folder(t, some(child))
	sibling := e.folder(t, uuid.NullUUID{})
	got := e.mustDelete(t, "example_folders", root)
	wantRows(t, "tree", got.Rows, map[string][]uuid.UUID{"example_folders": {root, child, leaf}})
	if !e.active(t, "example_folders", sibling) {
		t.Error("an unrelated folder was deleted")
	}

	a := e.folder(t, uuid.NullUUID{})
	b := e.folder(t, some(a))
	if _, err := e.app.Pool().Exec(t.Context(), "UPDATE active_example_folders SET parent_id = $1 WHERE id = $2", b, a); err != nil {
		t.Fatal(err)
	}
	cycle := e.mustDelete(t, "example_folders", a)
	wantRows(t, "cycle", cycle.Rows, map[string][]uuid.UUID{"example_folders": {a, b}})

	restored, err := e.restore(t, got.DeletionID)
	if err != nil {
		t.Fatal(err)
	}
	wantRows(t, "restored tree", restored.Rows, map[string][]uuid.UUID{"example_folders": {root, child, leaf}})
	e.noOrphans(t)
}

// An exempt (M1) child on a restrict edge holds its parent: a receipt is
// never deleted, so its project cannot be.
func TestRestrictOnAnExemptChild(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	owner := e.owner(t)
	project := e.project(t, owner)
	e.receipt(t, project)

	_, err := e.delete(t, "example_projects", project, system)
	wantCode(t, err, softdelete.CodeRestricted)
	_, err = e.delete(t, "example_owners", owner, system)
	wantCode(t, err, softdelete.CodeRestricted)
	if !e.active(t, "example_projects", project) {
		t.Error("project deleted under a receipt")
	}
	e.noOrphans(t)
}

// A row whose parent on a restrict edge was deleted meanwhile cannot come
// back either.
func TestRestoreParentDeletedOnRestrictEdge(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	project := e.project(t, e.owner(t))
	invoice := e.invoice(t, project, uuid.NullUUID{})
	invoiceDeletion := e.mustDelete(t, "example_invoices", invoice)
	e.mustDelete(t, "example_projects", project)

	_, err := e.restore(t, invoiceDeletion.DeletionID)
	wantCode(t, err, softdelete.CodeRestoreParentDeleted)
	if e.active(t, "example_invoices", invoice) {
		t.Error("invoice restored under a deleted project")
	}
	e.noOrphans(t)
}

// A deletion whose root row is gone (purged) cannot be restored.
func TestRestoreExpired(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	note := e.insert(t, "INSERT INTO active_example_notes (id, body) VALUES ($1, 'n')")
	deleted := e.mustDelete(t, "example_notes", note)
	if _, err := e.maint.Pool().Exec(t.Context(), "DELETE FROM example_notes WHERE id = $1", note); err != nil {
		t.Fatal(err)
	}
	_, err := e.restore(t, deleted.DeletionID)
	wantCode(t, err, softdelete.CodeRestoreExpired)
}

func TestLockActiveParent(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	project := e.project(t, e.owner(t))
	deleted := e.project(t, e.owner(t))
	e.mustDelete(t, "example_projects", deleted)

	lock := func(id uuid.UUID) error {
		return e.app.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
			return softdelete.LockActiveParent(ctx, q, "example_projects", id)
		})
	}
	if err := lock(project); err != nil {
		t.Errorf("active parent: %v", err)
	}
	for name, id := range map[string]uuid.UUID{"deleted": deleted, "missing": newID()} {
		if err := lock(id); !errors.Is(err, softdelete.ErrNotActive) {
			t.Errorf("%s parent: err = %v, want ErrNotActive", name, err)
		}
	}
}

// Concurrency (ADR-0020 Karar 6). Each test holds one transaction open at
// a known point, starts the other, waits until Postgres reports it waiting
// for a lock, then lets the first commit. Without the locks the second
// transaction would not wait and the invariant would break.

// held is a transaction kept open after its work until release.
type held struct {
	done    <-chan error
	release func()
}

// hold runs fn in a transaction of pool and keeps the transaction open
// after fn returned, until release (or the end of the test).
func hold(t *testing.T, pool *db.DB, fn func(ctx context.Context, q db.Querier) error) held {
	t.Helper()
	ready := make(chan error, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- pool.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
			err := fn(ctx, q)
			ready <- err
			if err != nil {
				return err
			}
			<-release
			return nil
		})
	}()
	if err := <-ready; err != nil {
		t.Fatalf("held transaction: %v", err)
	}
	once := sync.OnceFunc(func() { close(release) })
	t.Cleanup(once)
	return held{done: done, release: once}
}

// background runs fn in a transaction of pool and returns its backend's
// pid and its result.
func background(t *testing.T, pool *db.DB, fn func(ctx context.Context, q db.Querier) error) (int32, <-chan error) {
	t.Helper()
	pids := make(chan int32, 1)
	done := make(chan error, 1)
	go func() {
		done <- pool.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
			var pid int32
			if err := q.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				pids <- 0
				return err
			}
			pids <- pid
			return fn(ctx, q)
		})
	}()
	pid := <-pids
	if pid == 0 {
		t.Fatalf("background transaction: %v", <-done)
	}
	return pid, done
}

// waitBlocked waits until backend pid waits for a lock. It fails when the
// transaction finishes without waiting: then nothing serialized it.
func (e env) waitBlocked(t *testing.T, pid int32, done <-chan error) {
	t.Helper()
	dbtest.Eventually(t, 10*time.Second, "the second transaction to wait for a lock", func() (bool, error) {
		select {
		case err := <-done:
			return false, fmt.Errorf("it finished without waiting: %w", err)
		default:
		}
		var waiting bool
		err := e.owner.Pool().QueryRow(t.Context(),
			"SELECT EXISTS (SELECT FROM pg_locks WHERE pid = $1 AND NOT granted)", pid).Scan(&waiting)
		return waiting, err
	})
}

// A Restore that reopens a child while a Delete of its parent is
// uncommitted waits for the Delete and then refuses: the parent is deleted.
func TestRestoreWaitsForConcurrentDelete(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	project := e.project(t, e.owner(t))
	task := e.task(t, project, "a")
	taskDeletion := e.mustDelete(t, "example_tasks", task)

	deleting := hold(t, e.app, func(ctx context.Context, q db.Querier) error {
		_, err := e.del.Delete(ctx, q, deleteRequest("example_projects", project, system))
		return err
	})
	pid, restoring := background(t, e.maint, func(ctx context.Context, q db.Querier) error {
		_, err := e.del.Restore(ctx, q, e.restoreRequest(taskDeletion.DeletionID))
		return err
	})
	e.waitBlocked(t, pid, restoring)
	deleting.release()
	if err := <-deleting.done; err != nil {
		t.Fatalf("delete: %v", err)
	}
	wantCode(t, <-restoring, softdelete.CodeRestoreParentDeleted)
	if e.active(t, "example_tasks", task) {
		t.Error("task restored under a project deleted concurrently")
	}
	e.noOrphans(t)
}

// A Delete of a parent while a Restore of its restrict child is uncommitted
// waits for the Restore and then sees the child: 409.
func TestDeleteWaitsForConcurrentRestore(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	project := e.project(t, e.owner(t))
	invoice := e.invoice(t, project, uuid.NullUUID{})
	invoiceDeletion := e.mustDelete(t, "example_invoices", invoice)

	restoring := hold(t, e.maint, func(ctx context.Context, q db.Querier) error {
		_, err := e.del.Restore(ctx, q, e.restoreRequest(invoiceDeletion.DeletionID))
		return err
	})
	pid, deleting := background(t, e.app, func(ctx context.Context, q db.Querier) error {
		_, err := e.del.Delete(ctx, q, deleteRequest("example_projects", project, system))
		return err
	})
	e.waitBlocked(t, pid, deleting)
	restoring.release()
	if err := <-restoring.done; err != nil {
		t.Fatalf("restore: %v", err)
	}
	wantCode(t, <-deleting, softdelete.CodeRestricted)
	if !e.active(t, "example_projects", project) || !e.active(t, "example_invoices", invoice) {
		t.Error("project deleted while its invoice came back")
	}
	e.noOrphans(t)
}

// LockActiveParent while a Delete of the parent is uncommitted waits and
// then reports the parent deleted.
func TestLockActiveParentWaitsForConcurrentDelete(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	project := e.project(t, e.owner(t))
	deleting := hold(t, e.app, func(ctx context.Context, q db.Querier) error {
		_, err := e.del.Delete(ctx, q, deleteRequest("example_projects", project, system))
		return err
	})
	pid, locking := background(t, e.app, func(ctx context.Context, q db.Querier) error {
		return softdelete.LockActiveParent(ctx, q, "example_projects", project)
	})
	e.waitBlocked(t, pid, locking)
	deleting.release()
	if err := <-deleting.done; err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := <-locking; !errors.Is(err, softdelete.ErrNotActive) {
		t.Fatalf("lock after the delete: err = %v, want ErrNotActive", err)
	}
}

// A child inserted under LockActiveParent while a Delete cascades towards
// its parent makes the Delete wait; the Delete then takes the new child
// along.
func TestDeleteCascadesToAChildInsertedConcurrently(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	owner := e.owner(t)
	project := e.project(t, owner)
	newTask := newID()
	inserting := hold(t, e.app, func(ctx context.Context, q db.Querier) error {
		if err := softdelete.LockActiveParent(ctx, q, "example_projects", project); err != nil {
			return err
		}
		_, err := q.Exec(ctx, "INSERT INTO active_example_tasks (id, project_id, name) VALUES ($1, $2, 'new')", newTask, project)
		return err
	})
	var deleted softdelete.Deleted
	pid, deleting := background(t, e.app, func(ctx context.Context, q db.Querier) error {
		var err error
		deleted, err = e.del.Delete(ctx, q, deleteRequest("example_owners", owner, system))
		return err
	})
	e.waitBlocked(t, pid, deleting)
	inserting.release()
	if err := <-inserting.done; err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := <-deleting; err != nil {
		t.Fatalf("delete: %v", err)
	}
	if e.active(t, "example_tasks", newTask) {
		t.Error("the task inserted concurrently survived its project's deletion")
	}
	if d := e.deletionOf(t, "example_tasks", newTask); d.UUID != deleted.DeletionID {
		t.Errorf("new task deletion_id = %v, want %s", d, deleted.DeletionID)
	}
	e.noOrphans(t)
}
