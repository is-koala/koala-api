//go:build integration

package bootstrap_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/db/bootstrap"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
)

// TestRolesSQLTakesLoginAway: a group role found with LOGIN is switched
// back to NOLOGIN, and running the SQL twice changes nothing else.
func TestRolesSQLTakesLoginAway(t *testing.T) {
	t.Parallel()

	dbtest.New(t) // the roles exist from here on
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, dbtest.AdminURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx := context.WithoutCancel(ctx)
		_, _ = conn.Exec(cctx, "ALTER ROLE koala_maint NOLOGIN")
		_ = conn.Close(cctx)
	})

	if _, err := conn.Exec(ctx, "ALTER ROLE koala_maint LOGIN"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := conn.Exec(ctx, bootstrap.RolesSQL); err != nil {
			t.Fatalf("roles.sql: %v", err)
		}
	}
	var withLogin int
	err = conn.QueryRow(ctx,
		"SELECT count(*) FROM pg_roles WHERE rolname IN ('koala_owner', 'koala_app', 'koala_maint') AND rolcanlogin").Scan(&withLogin)
	if err != nil || withLogin != 0 {
		t.Fatalf("group roles with LOGIN = %d, err = %v; want 0", withLogin, err)
	}
}
