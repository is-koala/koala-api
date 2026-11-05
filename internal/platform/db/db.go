// Package db is the Postgres platform (ADR-0012, ADR-0017, plan §3.5): the
// pgx pool, the readiness check, the transaction helper InTx and the goose
// migrator.
//
// Every connection runs SET ROLE to one of the group roles of ADR-0013 when
// it opens, and the pool checks current_user each time it hands a
// connection out. SET ROLE narrows what a session does by accident; it is
// not a security boundary, because the session can RESET ROLE back to the
// login. The boundary is the login itself: the server's login must be a
// member of koala_app only, which Open enforces for RoleApp
// (ErrLoginTooPrivileged); owner and maintenance logins must not be
// superusers (ErrSuperuserLogin).
//
// Because the role and the settings live on the session, application code
// never changes session state: no session-level SET, only SET LOCAL inside
// InTx. A connection whose current_user has changed is destroyed, not
// reused.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Group roles (db/bootstrap/roles.sql).
const (
	// RoleOwner owns the schema; migrations and maintenance DDL run as it.
	RoleOwner = "koala_owner"
	// RoleApp is the application. It reads and writes active_<t> views and
	// River's tables, never soft-deleted base tables (ADR-0013).
	RoleApp = "koala_app"
	// RoleMaint restores, cleans up and anonymizes on the base tables.
	RoleMaint = "koala_maint"
)

// ErrLoginTooPrivileged means the login behind a RoleApp pool could do more
// than koala_app (see checkAppLogin). The pool refuses every connection
// with it.
var ErrLoginTooPrivileged = errors.New("db: login is more privileged than koala_app")

// ErrSuperuserLogin means a superuser login was given for an owner,
// maintenance or bootstrap connection.
var ErrSuperuserLogin = errors.New("db: superuser login refused; use a login with only the needed membership or attribute")

// ErrRoleChanged means a pooled connection no longer acts as its pool's
// role, because something ran a session-level SET ROLE or RESET ROLE. The
// connection is destroyed.
var ErrRoleChanged = errors.New("db: connection's current_user changed; session-level SET is forbidden, use SET LOCAL")

// readyTimeout bounds the readiness ping, so /readyz answers 503 quickly
// when the database is unreachable instead of waiting for a TCP timeout.
const readyTimeout = 2 * time.Second

// sessionSettings are the per-role defaults of a pool (ADR-0017). A value
// in the URL wins: pool_max_conns, application_name or the setting itself.
type sessionSettings struct {
	maxConns int32
	// statementTimeout and idleInTx are Postgres durations; 0 disables.
	statementTimeout time.Duration
	idleInTx         time.Duration
}

func settingsFor(role string) sessionSettings {
	switch role {
	case RoleApp:
		// Below httpx.RequestTimeout (25s), so the database cancels a slow
		// statement before the request times out. A River worker that
		// needs longer statements works in batches.
		return sessionSettings{maxConns: 10, statementTimeout: 20 * time.Second, idleInTx: 10 * time.Second}
	case RoleMaint:
		return sessionSettings{maxConns: 2, statementTimeout: 5 * time.Minute, idleInTx: time.Minute}
	default: // RoleOwner: migrations and REINDEX CONCURRENTLY may run long.
		return sessionSettings{maxConns: 2, statementTimeout: 0, idleInTx: time.Minute}
	}
}

// Config configures Open.
type Config struct {
	// URL is a postgres:// URL. Never logged.
	URL string
	// Role is the group role every connection acts as.
	Role string
	// Logger receives AfterCommit hook panics.
	Logger *slog.Logger
}

// DB is a connection pool whose connections act as one group role.
type DB struct {
	pool   *pgxpool.Pool
	role   string
	logger *slog.Logger
}

// Open returns a pool for cfg.URL whose connections act as cfg.Role. The
// pool connects lazily: Open does not fail when the database is down, Ready
// does. Every new connection checks its login first: for RoleApp against
// ErrLoginTooPrivileged, for the other roles against ErrSuperuserLogin.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.Logger == nil {
		return nil, errors.New("db: Logger is required")
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgx redacts passwords only on a best-effort basis; never let the
		// URL reach a log.
		return nil, errors.New("parse database URL: invalid connection string")
	}
	s := settingsFor(cfg.Role)
	if !strings.Contains(cfg.URL, "pool_max_conns") {
		poolCfg.MaxConns = s.maxConns
	}
	applyRuntimeDefaults(poolCfg.ConnConfig, cfg.Role, s)

	setRole := setRoleHook(cfg.Role)
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		check := CheckNotSuperuser
		if cfg.Role == RoleApp {
			check = checkAppLogin
		}
		if err := check(ctx, conn); err != nil {
			return err
		}
		return setRole(ctx, conn)
	}
	poolCfg.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		var current string
		if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&current); err != nil {
			return false, fmt.Errorf("check current_user: %w", err)
		}
		if current != cfg.Role {
			return false, fmt.Errorf("%w: want %s, got %s", ErrRoleChanged, cfg.Role, current)
		}
		return true, nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return &DB{pool: pool, role: cfg.Role, logger: cfg.Logger}, nil
}

// applyRuntimeDefaults sets application_name, statement_timeout and
// idle_in_transaction_session_timeout as startup parameters unless the URL
// already sets them.
func applyRuntimeDefaults(cc *pgx.ConnConfig, role string, s sessionSettings) {
	defaults := map[string]string{
		"application_name":                    "koala-api/" + strings.TrimPrefix(role, "koala_"),
		"statement_timeout":                   strconv.FormatInt(s.statementTimeout.Milliseconds(), 10),
		"idle_in_transaction_session_timeout": strconv.FormatInt(s.idleInTx.Milliseconds(), 10),
	}
	for k, v := range defaults {
		if _, ok := cc.RuntimeParams[k]; !ok {
			cc.RuntimeParams[k] = v
		}
	}
}

// setRoleHook returns a connect hook that switches the session to role.
// This is the one session-level SET in the codebase.
func setRoleHook(role string) func(context.Context, *pgx.Conn) error {
	stmt := "SET ROLE " + pgx.Identifier{role}.Sanitize()
	return func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("set role %s: %w", role, err)
		}
		return nil
	}
}

// checkAppLogin admits a login for a RoleApp pool only if it can do
// exactly what koala_app can (an allow list, not a deny list): its only
// role membership, direct or inherited, is koala_app; it has none of the
// SUPERUSER, BYPASSRLS, CREATEROLE, CREATEDB or REPLICATION attributes; and
// it holds no privilege of its own on any table, view, sequence or column
// of this database and owns none.
func checkAppLogin(ctx context.Context, conn *pgx.Conn) error {
	var login string
	var attrs, memberships []string
	var ownPrivileges bool
	err := conn.QueryRow(ctx, `
		SELECT r.rolname::text,
		       array_remove(ARRAY[
		           CASE WHEN r.rolsuper THEN 'SUPERUSER' END,
		           CASE WHEN r.rolbypassrls THEN 'BYPASSRLS' END,
		           CASE WHEN r.rolcreaterole THEN 'CREATEROLE' END,
		           CASE WHEN r.rolcreatedb THEN 'CREATEDB' END,
		           CASE WHEN r.rolreplication THEN 'REPLICATION' END
		       ], NULL),
		       ARRAY(SELECT g.rolname::text FROM pg_roles g
		             WHERE g.oid <> r.oid AND pg_has_role(r.oid, g.oid, 'MEMBER')
		             ORDER BY 1),
		       EXISTS (SELECT FROM pg_class c WHERE c.relowner = r.oid)
		       OR EXISTS (SELECT FROM pg_class c, aclexplode(c.relacl) x WHERE x.grantee = r.oid)
		       OR EXISTS (SELECT FROM pg_attribute a, aclexplode(a.attacl) x
		                  WHERE a.attacl IS NOT NULL AND x.grantee = r.oid)
		FROM pg_roles r WHERE r.rolname = session_user`).
		Scan(&login, &attrs, &memberships, &ownPrivileges)
	if err != nil {
		return fmt.Errorf("check login privileges: %w", err)
	}
	var why []string
	if len(attrs) > 0 {
		why = append(why, "has "+strings.Join(attrs, ", "))
	}
	if len(memberships) != 1 || memberships[0] != RoleApp {
		why = append(why, fmt.Sprintf("is a member of %v, want exactly [%s]", memberships, RoleApp))
	}
	if ownPrivileges {
		why = append(why, "owns or holds privileges on relations of its own")
	}
	if len(why) > 0 {
		return fmt.Errorf("%w: %s %s", ErrLoginTooPrivileged, login, strings.Join(why, "; "))
	}
	return nil
}

// CheckNotSuperuser rejects a superuser login. The owner, maintenance and
// bootstrap logins need specific powers (membership in koala_owner or
// koala_maint, CREATEROLE), never all of them (ADR-0017).
func CheckNotSuperuser(ctx context.Context, conn *pgx.Conn) error {
	var login string
	var super bool
	err := conn.QueryRow(ctx, "SELECT rolname::text, rolsuper FROM pg_roles WHERE rolname = session_user").Scan(&login, &super)
	if err != nil {
		return fmt.Errorf("check login privileges: %w", err)
	}
	if super {
		return fmt.Errorf("%w: %s", ErrSuperuserLogin, login)
	}
	return nil
}

// Pool returns the underlying pool for reads outside a transaction (sqlc
// queries take it as their DBTX) and for River's driver.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// Ready pings the database. It is the readiness check of /readyz. The error
// wraps ErrLoginTooPrivileged when the login fails that check.
func (d *DB) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := d.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// Close waits for acquired connections to be released and closes the pool.
// Call it after the HTTP server and the workers have stopped.
func (d *DB) Close() { d.pool.Close() }
