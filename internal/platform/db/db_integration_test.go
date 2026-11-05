//go:build integration

package db_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
)

func countNotes(ctx context.Context, t *testing.T, database *db.DB, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := database.Pool().QueryRow(ctx,
		"SELECT count(*) FROM active_example_notes WHERE id = $1", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func insertNote(ctx context.Context, q db.Querier, id uuid.UUID) error {
	_, err := q.Exec(ctx, "INSERT INTO active_example_notes (id, body) VALUES ($1, 'note')", id)
	return err
}

// TestConnectionsActAsTheirRole: every connection runs as its group role
// through a separate login.
func TestConnectionsActAsTheirRole(t *testing.T) {
	t.Parallel()

	d := dbtest.New(t)
	for _, role := range []string{db.RoleOwner, db.RoleApp, db.RoleMaint} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			var current, session, app string
			err := d.Open(t, role).Pool().QueryRow(t.Context(),
				"SELECT current_user, session_user, current_setting('application_name')").Scan(&current, &session, &app)
			if err != nil {
				t.Fatal(err)
			}
			if current != role || session == role {
				t.Fatalf("current_user = %s, session_user = %s; want current %s through a separate login", current, session, role)
			}
			if !strings.HasPrefix(app, "koala-api/") {
				t.Fatalf("application_name = %q, want koala-api/<role>", app)
			}
		})
	}
}

// TestAppPoolSessionSettings: the server's pool runs with the timeouts of
// ADR-0017.
func TestAppPoolSessionSettings(t *testing.T) {
	t.Parallel()

	app := dbtest.New(t).Open(t, db.RoleApp)
	var stmt, idle string
	err := app.Pool().QueryRow(t.Context(),
		"SELECT current_setting('statement_timeout'), current_setting('idle_in_transaction_session_timeout')").Scan(&stmt, &idle)
	if err != nil {
		t.Fatal(err)
	}
	if stmt != "20s" || idle != "10s" {
		t.Fatalf("statement_timeout = %s, idle_in_transaction_session_timeout = %s; want 20s, 10s", stmt, idle)
	}
}

// TestAppLoginMustNotBeMorePrivileged: SET ROLE is not a boundary, so a
// RoleApp pool admits only a login that can do exactly what koala_app can
// (allow list): membership exactly {koala_app}, no powerful attributes, no
// privileges or objects of its own.
func TestAppLoginMustNotBeMorePrivileged(t *testing.T) {
	t.Parallel()

	d := dbtest.New(t)
	owner := d.Open(t, db.RoleOwner)
	app := []string{db.RoleApp}
	tests := []struct {
		name        string
		login       dbtest.LoginOptions
		directGrant bool
		refused     bool
	}{
		{"member of koala_app only", dbtest.LoginOptions{Roles: app}, false, false},
		{"also member of koala_maint", dbtest.LoginOptions{Roles: []string{db.RoleApp, db.RoleMaint}}, false, true},
		{"also member of koala_owner", dbtest.LoginOptions{Roles: []string{db.RoleApp, db.RoleOwner}}, false, true},
		{"also member of pg_write_all_data", dbtest.LoginOptions{Roles: []string{db.RoleApp, "pg_write_all_data"}}, false, true},
		{"also member of pg_read_all_data", dbtest.LoginOptions{Roles: []string{db.RoleApp, "pg_read_all_data"}}, false, true},
		{"BYPASSRLS", dbtest.LoginOptions{Attributes: []string{"BYPASSRLS"}, Roles: app}, false, true},
		{"CREATEROLE", dbtest.LoginOptions{Attributes: []string{"CREATEROLE"}, Roles: app}, false, true},
		{"CREATEDB", dbtest.LoginOptions{Attributes: []string{"CREATEDB"}, Roles: app}, false, true},
		{"REPLICATION", dbtest.LoginOptions{Attributes: []string{"REPLICATION"}, Roles: app}, false, true},
		{"SUPERUSER", dbtest.LoginOptions{Attributes: []string{"SUPERUSER"}}, false, true},
		{"direct privilege on a base table", dbtest.LoginOptions{Roles: app}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			login := d.NewLogin(t, tt.login)
			if tt.directGrant {
				user := pgx.Identifier{login.User}.Sanitize()
				if _, err := owner.Pool().Exec(ctx, "GRANT SELECT ON example_notes TO "+user); err != nil {
					t.Fatal(err)
				}
				// Runs before the login is dropped: a role with privileges
				// cannot be dropped.
				t.Cleanup(func() {
					_, _ = owner.Pool().Exec(context.WithoutCancel(ctx), "REVOKE ALL ON example_notes FROM "+user)
				})
			}
			database, err := db.Open(ctx, db.Config{URL: login.URL, Role: db.RoleApp, Logger: discard()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(database.Close)

			err = database.Ready(ctx)
			if refused := errors.Is(err, db.ErrLoginTooPrivileged); refused != tt.refused {
				t.Fatalf("Ready() = %v; refused = %v, want %v", err, refused, tt.refused)
			}
			if !tt.refused && err != nil {
				t.Fatalf("Ready() = %v", err)
			}
		})
	}
}

// TestOwnerAndMaintPoolsRefuseSuperuser: owner and maintenance logins need
// one membership, never every power.
func TestOwnerAndMaintPoolsRefuseSuperuser(t *testing.T) {
	t.Parallel()

	d := dbtest.New(t)
	login := d.NewLogin(t, dbtest.LoginOptions{Attributes: []string{"SUPERUSER"}})
	for _, role := range []string{db.RoleOwner, db.RoleMaint} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			database, err := db.Open(t.Context(), db.Config{URL: login.URL, Role: role, Logger: discard()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(database.Close)
			if err := database.Ready(t.Context()); !errors.Is(err, db.ErrSuperuserLogin) {
				t.Fatalf("Ready() = %v, want ErrSuperuserLogin", err)
			}
		})
	}
}

// TestSessionLevelRoleChangeIsCaught: a session-level RESET ROLE survives
// on a pooled connection; the pool notices on the next acquire, fails that
// query and replaces the connection.
func TestSessionLevelRoleChangeIsCaught(t *testing.T) {
	t.Parallel()

	d := dbtest.New(t)
	database, err := db.Open(t.Context(), db.Config{URL: d.URL(db.RoleApp) + "?pool_max_conns=1", Role: db.RoleApp, Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	ctx := t.Context()

	conn, err := database.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, "RESET ROLE")
	conn.Release()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := database.Pool().Exec(ctx, "SELECT 1"); !errors.Is(err, db.ErrRoleChanged) {
		t.Fatalf("query on the changed connection = %v, want ErrRoleChanged", err)
	}
	var current string
	if err := database.Pool().QueryRow(ctx, "SELECT current_user").Scan(&current); err != nil || current != db.RoleApp {
		t.Fatalf("next connection: current_user = %q, err = %v; want %s", current, err, db.RoleApp)
	}
}

func TestReady(t *testing.T) {
	t.Parallel()

	if err := dbtest.New(t).Open(t, db.RoleApp).Ready(t.Context()); err != nil {
		t.Fatalf("Ready() = %v", err)
	}
}

func TestInTx(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	tests := []struct {
		name      string
		fn        func(ctx context.Context, q db.Querier, id uuid.UUID) error
		wantErr   error
		wantPanic bool
		wantRows  int
	}{
		{
			name:     "commit",
			fn:       insertNote,
			wantRows: 1,
		},
		{
			name: "error rolls back and is returned unchanged",
			fn: func(ctx context.Context, q db.Querier, id uuid.UUID) error {
				if err := insertNote(ctx, q, id); err != nil {
					return err
				}
				return errBoom
			},
			wantErr: errBoom,
		},
		{
			name: "panic rolls back and propagates",
			fn: func(ctx context.Context, q db.Querier, id uuid.UUID) error {
				if err := insertNote(ctx, q, id); err != nil {
					return err
				}
				panic("boom")
			},
			wantPanic: true,
		},
		{
			name: "SET LOCAL ends with the transaction",
			fn: func(ctx context.Context, q db.Querier, id uuid.UUID) error {
				if _, err := q.Exec(ctx, "SET LOCAL statement_timeout = '1s'"); err != nil {
					return err
				}
				return insertNote(ctx, q, id)
			},
			wantRows: 1,
		},
	}

	app := dbtest.New(t).Open(t, db.RoleApp)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			id := uuid.Must(uuid.NewV7())

			var err error
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				err = app.InTx(ctx, func(ctx context.Context, q db.Querier) error { return tt.fn(ctx, q, id) })
				return false
			}()

			if panicked != tt.wantPanic {
				t.Fatalf("panicked = %v, want %v", panicked, tt.wantPanic)
			}
			if !tt.wantPanic && !errors.Is(err, tt.wantErr) {
				t.Fatalf("InTx() = %v, want %v", err, tt.wantErr)
			}
			if got := countNotes(ctx, t, app, id); got != tt.wantRows {
				t.Fatalf("rows = %d, want %d", got, tt.wantRows)
			}
		})
	}
}

// TestInTxRejectsNesting: InTx inside a callback would run an independent
// transaction on a second connection.
func TestInTxRejectsNesting(t *testing.T) {
	t.Parallel()

	app := dbtest.New(t).Open(t, db.RoleApp)
	ctx := t.Context()
	id := uuid.Must(uuid.NewV7())

	err := app.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		if err := insertNote(ctx, q, id); err != nil {
			return err
		}
		return app.InTx(ctx, func(context.Context, db.Querier) error { return nil })
	})
	if !errors.Is(err, db.ErrNestedTx) {
		t.Fatalf("InTx() = %v, want ErrNestedTx", err)
	}
	if got := countNotes(ctx, t, app, id); got != 0 {
		t.Fatalf("rows = %d, want the outer transaction rolled back", got)
	}
}

// TestAfterCommit: hooks run after the commit (the row is visible from
// another connection) and never after a rollback.
func TestAfterCommit(t *testing.T) {
	t.Parallel()

	app := dbtest.New(t).Open(t, db.RoleApp)
	ctx := t.Context()

	for _, commit := range []bool{true, false} {
		id := uuid.Must(uuid.NewV7())
		ran, seen := false, 0
		_ = app.InTx(ctx, func(ctx context.Context, q db.Querier) error {
			q.AfterCommit(func(hookCtx context.Context) {
				ran = true
				seen = countNotes(hookCtx, t, app, id)
			})
			if err := insertNote(ctx, q, id); err != nil {
				return err
			}
			if !commit {
				return errors.New("roll back")
			}
			return nil
		})
		if ran != commit {
			t.Fatalf("commit=%v: hook ran = %v", commit, ran)
		}
		if commit && seen != 1 {
			t.Fatalf("hook saw %d rows from another connection, want the committed row", seen)
		}
	}
}

// TestAfterCommitHookPanic: a panicking hook is logged; InTx still
// succeeds and the hooks after it run.
func TestAfterCommitHookPanic(t *testing.T) {
	t.Parallel()

	d := dbtest.New(t)
	var logs bytes.Buffer
	app, err := db.Open(t.Context(), db.Config{
		URL: d.URL(db.RoleApp), Role: db.RoleApp, Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	ctx := t.Context()

	var ran []int
	err = app.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		q.AfterCommit(func(context.Context) { ran = append(ran, 1) })
		q.AfterCommit(func(context.Context) { panic("broadcast failed") })
		q.AfterCommit(func(context.Context) { ran = append(ran, 3) })
		return insertNote(ctx, q, uuid.Must(uuid.NewV7()))
	})
	if err != nil {
		t.Fatalf("InTx() = %v", err)
	}
	if len(ran) != 2 || ran[0] != 1 || ran[1] != 3 {
		t.Fatalf("hooks ran = %v, want [1 3]", ran)
	}
	if !strings.Contains(logs.String(), "after-commit hook panicked") || !strings.Contains(logs.String(), "broadcast failed") {
		t.Fatalf("panic not logged: %s", logs.String())
	}
}

// TestPgxTxCannotEndTheTransaction: a library handed q.PgxTx() cannot
// commit or roll back behind InTx's back; savepoints still work.
func TestPgxTxCannotEndTheTransaction(t *testing.T) {
	t.Parallel()

	app := dbtest.New(t).Open(t, db.RoleApp)
	ctx := t.Context()
	id := uuid.Must(uuid.NewV7())

	err := app.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		tx := q.PgxTx()
		if err := tx.Commit(ctx); !errors.Is(err, db.ErrTxManaged) {
			t.Errorf("Commit() = %v, want ErrTxManaged", err)
		}
		if err := tx.Rollback(ctx); !errors.Is(err, db.ErrTxManaged) {
			t.Errorf("Rollback() = %v, want ErrTxManaged", err)
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := sp.Exec(ctx, "INSERT INTO active_example_notes (id, body) VALUES ($1, 'in savepoint')", id); err != nil {
			return err
		}
		return sp.Commit(ctx) // releases the savepoint only
	})
	if err != nil {
		t.Fatalf("InTx() = %v", err)
	}
	if got := countNotes(ctx, t, app, id); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
}
