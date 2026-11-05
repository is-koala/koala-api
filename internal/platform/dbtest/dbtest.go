// Package dbtest gives each integration test its own Postgres database
// (plan §6.1 Faz 1, "test başına DB (template DB)"; ADR-0017).
//
// The server is the one the libpq environment points at (PGHOST, PGPORT,
// PGUSER, PGPASSWORD, PGSSLMODE); in CI that is the postgres service
// container of verify.yml. PGUSER must be a superuser: the harness creates
// databases and roles, and some tests create privileged logins. Tests that
// use this package carry the `integration` build tag and run with
// make test-integration.
//
// A template database holds the group roles' schema: every migration in
// db/migrations plus the example migrations in testdata, applied by a login
// that is a member of koala_owner through the same db.Migrator that
// `koala migrate up` uses. Its name contains a hash of that SQL; a test
// process that finds no template for its hash builds one. Each test clones
// the template (CREATE DATABASE … TEMPLATE, a file copy of a small
// database) and gets fresh login users, one per group role, with generated
// passwords. Cleanup drops both.
//
// CREATE DATABASE does not copy database-level privileges, so the clone
// gets the template's database ACL replayed (REVOKE/GRANT); everything
// inside the database, including default privileges, is copied as is.
//
// Concurrency: building or dropping templates and sweeping leftovers take
// an exclusive advisory lock; cloning takes the same lock shared, so a
// template is never dropped while it is being copied, also when two
// schema versions (two branches) share a server. Databases and logins
// carry their creation time in their names; one older than an hour is a
// leftover of a killed run and is dropped by the next run.
package dbtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/db/bootstrap"
	"github.com/is-koala/koala-api/db/migrations"
	"github.com/is-koala/koala-api/internal/platform/db"
)

// examples are migrations that exist only in test databases: the ADR-0013
// soft-delete pattern on an example table and a NO TRANSACTION
// (CREATE INDEX CONCURRENTLY) migration. Their versions start at 9001 so
// they apply after the real ones.
//
//go:embed testdata/migrations/*.sql
var examples embed.FS

const (
	// lockKey guards role bootstrap, sweeps and templates across the test
	// processes `go test ./...` runs in parallel.
	lockKey = 0x6b6f616c61 // "koala"

	templatePrefix = "koala_tpl_"
	databasePrefix = "koala_t_"
	loginPrefix    = "kt_"

	// staleAfter is far beyond any test run (the CI job times out after
	// 30 minutes); anything older belongs to a killed run.
	staleAfter     = time.Hour
	cleanupTimeout = time.Minute
	cloneAttempts  = 3
)

// roles maps each group role to the suffix of its per-test login.
var roles = map[string]string{ //nolint:gochecknoglobals // read-only table
	db.RoleOwner: "owner",
	db.RoleApp:   "app",
	db.RoleMaint: "maint",
}

// DB is one test's database.
type DB struct {
	// Name is the database name.
	Name  string
	stamp string
	host  string
	port  uint16
	urls  map[string]string
}

// URL returns the connection URL of the login that is a member of role
// (db.RoleOwner, db.RoleApp or db.RoleMaint).
func (d DB) URL(role string) string {
	u, ok := d.urls[role]
	if !ok {
		panic("dbtest: unknown role " + role)
	}
	return u
}

// Open returns a pool that acts as role, closed when the test ends.
func (d DB) Open(t *testing.T, role string) *db.DB {
	t.Helper()
	database, err := db.Open(t.Context(), db.Config{URL: d.URL(role), Role: role, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("dbtest: open %s pool: %v", role, err)
	}
	t.Cleanup(database.Close)
	return database
}

// LoginOptions describes an extra login for a test.
type LoginOptions struct {
	// Attributes are role attributes such as SUPERUSER, BYPASSRLS,
	// CREATEROLE, CREATEDB or REPLICATION.
	Attributes []string
	// Roles the login is a member of (with INHERIT).
	Roles []string
}

// attributes are the role attributes LoginOptions accepts; they are
// written into SQL, so nothing else is allowed.
var attributes = []string{"SUPERUSER", "BYPASSRLS", "CREATEROLE", "CREATEDB", "REPLICATION"} //nolint:gochecknoglobals // read-only list

// Login is a login user and its URL to the test's database.
type Login struct {
	User     string
	URL      string
	password string
	host     string
	port     uint16
}

// URLFor returns the login's URL to another database of the server, such
// as postgres.
func (l Login) URLFor(dbName string) string {
	return loginURL(l.host, l.port, l.User, l.password, dbName)
}

// NewLogin creates an extra login for this test's database, dropped when
// the test ends.
func (d DB) NewLogin(t *testing.T, o LoginOptions) Login {
	t.Helper()
	ctx := t.Context()
	conn := connectAdmin(ctx, t)
	defer closeConn(ctx, conn)

	user, password := createLogin(ctx, t, conn, d.stamp+"_x"+randomHex(t, 2), o)
	t.Cleanup(func() {
		dropRoles(context.WithoutCancel(ctx), t, user)
	})
	return Login{User: user, URL: loginURL(d.host, d.port, user, password, d.Name), password: password, host: d.host, port: d.port}
}

// Disconnect makes user unable to reach the server, as if the instance
// using it had lost the database: the login can no longer log in and its
// sessions are terminated.
func (d DB) Disconnect(t *testing.T, user string) {
	t.Helper()
	ctx := t.Context()
	conn := connectAdmin(ctx, t)
	defer closeConn(ctx, conn)

	ident := pgx.Identifier{user}.Sanitize()
	exec(ctx, t, conn, "ALTER ROLE "+ident+" NOLOGIN")
	exec(ctx, t, conn, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1", user)
}

// AdminURL returns the URL of the PG* administrator, for tests of commands
// that need CREATEROLE (koala bootstrap).
func AdminURL(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	conn := connectAdmin(ctx, t)
	defer closeConn(ctx, conn)
	c := conn.Config()
	return loginURL(c.Host, c.Port, c.User, c.Password, c.Database)
}

// New returns a database with every migration applied, cloned from the
// template.
func New(t *testing.T) DB {
	t.Helper()
	ctx := t.Context()
	conn := connectAdmin(ctx, t)
	defer closeConn(ctx, conn)

	fsys := migrationFS(t)
	name := templateName(fsys)
	for range cloneAttempts {
		withLock(ctx, t, conn, false, func() {
			prepare(ctx, t, conn)
			ensureTemplate(ctx, t, conn, name, fsys)
		})
		var d DB
		cloned := false
		withLock(ctx, t, conn, true, func() {
			if templateReady(ctx, t, conn, name) {
				d = create(ctx, t, conn, name)
				cloned = true
			}
		})
		if cloned {
			return d
		}
		// A run of another schema version replaced the template between
		// the two locks; build it again.
	}
	t.Fatalf("dbtest: template %s kept disappearing; another schema version is testing on this server", name)
	return DB{}
}

// NewEmpty returns a database without migrations, owned by koala_owner, to
// test the migrations themselves.
func NewEmpty(t *testing.T) DB {
	t.Helper()
	ctx := t.Context()
	conn := connectAdmin(ctx, t)
	defer closeConn(ctx, conn)

	withLock(ctx, t, conn, false, func() { prepare(ctx, t, conn) })
	return create(ctx, t, conn, "template0")
}

func connectAdmin(ctx context.Context, t *testing.T) *pgx.Conn {
	t.Helper()
	// An empty connection string makes pgx read the libpq environment.
	conn, err := pgx.Connect(ctx, "")
	if err != nil {
		t.Fatalf("dbtest: connect to Postgres through PGHOST/PGPORT/PGUSER/PGPASSWORD (see make test-integration): %v", err)
	}
	return conn
}

// closeConn closes conn even when ctx (a test's context) is already done.
func closeConn(ctx context.Context, conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_ = conn.Close(ctx)
}

func exec(ctx context.Context, t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("dbtest: %s: %v", firstLine(sql), err)
	}
}

func firstLine(sql string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(sql), "\n")
	return line
}

// withLock runs fn holding the session advisory lock, exclusively or
// shared.
func withLock(ctx context.Context, t *testing.T, conn *pgx.Conn, shared bool, fn func()) {
	t.Helper()
	lock, unlock := "pg_advisory_lock", "pg_advisory_unlock"
	if shared {
		lock, unlock = "pg_advisory_lock_shared", "pg_advisory_unlock_shared"
	}
	exec(ctx, t, conn, "SELECT "+lock+"($1)", int64(lockKey))
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "SELECT "+unlock+"($1)", int64(lockKey))
	}()
	fn()
}

// prepare creates the group roles and drops leftovers of killed runs. It
// runs under the exclusive lock.
func prepare(ctx context.Context, t *testing.T, conn *pgx.Conn) {
	t.Helper()
	exec(ctx, t, conn, bootstrap.RolesSQL)

	cutoff := time.Now().Add(-staleAfter).Unix()
	for _, name := range names(ctx, t, conn, "SELECT datname FROM pg_database WHERE starts_with(datname, $1)", databasePrefix) {
		if stale(name, databasePrefix, cutoff) {
			exec(ctx, t, conn, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		}
	}
	var logins []string
	for _, name := range names(ctx, t, conn, "SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1)", loginPrefix) {
		if stale(name, loginPrefix, cutoff) {
			logins = append(logins, name)
		}
	}
	dropRoles(ctx, t, logins...)
}

func names(ctx context.Context, t *testing.T, conn *pgx.Conn, sql string, arg any) []string {
	t.Helper()
	rows, err := conn.Query(ctx, sql, arg)
	if err != nil {
		t.Fatalf("dbtest: %s: %v", sql, err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("dbtest: %s: %v", sql, err)
	}
	return list
}

// stale reports whether a prefix_<unix>_… name is older than cutoff. A
// name without a timestamp predates this scheme and is stale too.
func stale(name, prefix string, cutoff int64) bool {
	ts, _, _ := strings.Cut(strings.TrimPrefix(name, prefix), "_")
	created, err := strconv.ParseInt(ts, 10, 64)
	return err != nil || created < cutoff
}

// dropRoles drops logins; a failure is reported but does not stop the
// cleanup of the others.
func dropRoles(ctx context.Context, t *testing.T, users ...string) {
	t.Helper()
	if len(users) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	admin, err := pgx.Connect(ctx, "")
	if err != nil {
		t.Errorf("dbtest: connect to drop roles: %v", err)
		return
	}
	defer closeConn(ctx, admin)
	for _, user := range users {
		if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{user}.Sanitize()); err != nil {
			t.Errorf("dbtest: drop role %s: %v", user, err)
		}
	}
}

// migrationFS merges the real migrations and the examples into one tree.
func migrationFS(t *testing.T) fstest.MapFS {
	t.Helper()
	examplesRoot, err := fs.Sub(examples, "testdata/migrations")
	if err != nil {
		t.Fatal(err)
	}
	merged := fstest.MapFS{}
	for _, fsys := range []fs.FS{migrations.FS, examplesRoot} {
		entries, err := fs.ReadDir(fsys, ".")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if _, dup := merged[e.Name()]; dup {
				t.Fatalf("dbtest: migration %s exists twice", e.Name())
			}
			data, err := fs.ReadFile(fsys, e.Name())
			if err != nil {
				t.Fatal(err)
			}
			merged[e.Name()] = &fstest.MapFile{Data: data}
		}
	}
	return merged
}

// templateName hashes everything that shapes the template.
func templateName(fsys fstest.MapFS) string {
	h := sha256.New()
	h.Write([]byte(bootstrap.RolesSQL))
	files := make([]string, 0, len(fsys))
	for name := range fsys {
		files = append(files, name)
	}
	slices.Sort(files)
	for _, name := range files {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(fsys[name].Data)
		h.Write([]byte{0})
	}
	return templatePrefix + hex.EncodeToString(h.Sum(nil))[:16]
}

type templateRow struct {
	Name       string
	IsTemplate bool
}

func templateReady(ctx context.Context, t *testing.T, conn *pgx.Conn, name string) bool {
	t.Helper()
	var ready bool
	err := conn.QueryRow(ctx,
		"SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1 AND datistemplate)", name).Scan(&ready)
	if err != nil {
		t.Fatalf("dbtest: look up template: %v", err)
	}
	return ready
}

// ensureTemplate builds the template database unless a finished one with
// the same hash exists, and drops templates of other hashes and builds
// that died. It runs under the exclusive lock.
func ensureTemplate(ctx context.Context, t *testing.T, conn *pgx.Conn, name string, fsys fstest.MapFS) {
	t.Helper()
	rows, err := conn.Query(ctx,
		"SELECT datname, datistemplate FROM pg_database WHERE starts_with(datname, $1)", templatePrefix)
	if err != nil {
		t.Fatalf("dbtest: list templates: %v", err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[templateRow])
	if err != nil {
		t.Fatalf("dbtest: list templates: %v", err)
	}
	ready := false
	for _, r := range list {
		if r.Name == name && r.IsTemplate {
			ready = true
			continue
		}
		ident := pgx.Identifier{r.Name}.Sanitize()
		exec(ctx, t, conn, "ALTER DATABASE "+ident+" WITH IS_TEMPLATE false")
		exec(ctx, t, conn, "DROP DATABASE "+ident+" WITH (FORCE)")
	}
	if ready {
		return
	}

	ident := pgx.Identifier{name}.Sanitize()
	exec(ctx, t, conn, "CREATE DATABASE "+ident+" OWNER "+db.RoleOwner+" TEMPLATE template0")
	user, password := createLogin(ctx, t, conn, newStamp(t)+"_build", LoginOptions{Roles: []string{db.RoleOwner}})
	c := conn.Config()

	migrator, err := db.NewMigrator(loginURL(c.Host, c.Port, user, password, name), fsys, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("dbtest: %v", err)
	}
	_, upErr := migrator.Up(ctx)
	_ = migrator.Close()
	dropRoles(ctx, t, user)
	if upErr != nil {
		t.Fatalf("dbtest: build template: %v", upErr)
	}

	// CREATE DATABASE … TEMPLATE needs a template nobody is connected to.
	exec(ctx, t, conn, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", name)
	exec(ctx, t, conn, "ALTER DATABASE "+ident+" WITH IS_TEMPLATE true ALLOW_CONNECTIONS false")
}

type aclRow struct {
	Grantee   string
	Privilege string
}

// create makes a database from template plus one login per group role.
func create(ctx context.Context, t *testing.T, conn *pgx.Conn, template string) DB {
	t.Helper()
	c := conn.Config()
	stamp := newStamp(t)
	d := DB{Name: databasePrefix + stamp, stamp: stamp, host: c.Host, port: c.Port, urls: map[string]string{}}
	ident := pgx.Identifier{d.Name}.Sanitize()

	var logins []string
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		admin, err := pgx.Connect(ctx, "")
		if err != nil {
			t.Errorf("dbtest: cleanup connect: %v", err)
			return
		}
		defer closeConn(ctx, admin)
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("dbtest: drop database %s: %v", d.Name, err)
		}
		dropRoles(ctx, t, logins...)
	})

	exec(ctx, t, conn, "CREATE DATABASE "+ident+" TEMPLATE "+pgx.Identifier{template}.Sanitize()+" OWNER "+db.RoleOwner)
	if template != "template0" {
		copyDatabaseACL(ctx, t, conn, template, ident)
	}
	for role, short := range roles {
		user, password := createLogin(ctx, t, conn, stamp+"_"+short, LoginOptions{Roles: []string{role}})
		logins = append(logins, user)
		d.urls[role] = loginURL(c.Host, c.Port, user, password, d.Name)
	}
	return d
}

// copyDatabaseACL replays the template's database privileges on the clone
// (CREATE DATABASE does not copy them).
func copyDatabaseACL(ctx context.Context, t *testing.T, conn *pgx.Conn, template, ident string) {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT CASE WHEN x.grantee = 0 THEN 'PUBLIC' ELSE quote_ident(pg_get_userbyid(x.grantee)) END,
		       x.privilege_type
		FROM pg_database d, aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) x
		WHERE d.datname = $1 AND x.grantee <> d.datdba`, template)
	if err != nil {
		t.Fatalf("dbtest: read template ACL: %v", err)
	}
	grants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[aclRow])
	if err != nil {
		t.Fatalf("dbtest: read template ACL: %v", err)
	}
	exec(ctx, t, conn, "REVOKE ALL ON DATABASE "+ident+" FROM PUBLIC")
	for _, g := range grants {
		// Both values come from the catalog: privilege_type is a keyword,
		// the grantee is quote_ident'ed or PUBLIC.
		exec(ctx, t, conn, "GRANT "+g.Privilege+" ON DATABASE "+ident+" TO "+g.Grantee)
	}
}

// createLogin creates a login user with a generated password. The password
// lives only in this process.
func createLogin(ctx context.Context, t *testing.T, conn *pgx.Conn, suffix string, o LoginOptions) (user, password string) {
	t.Helper()
	user = loginPrefix + suffix
	password = randomHex(t, 16) // hex: safe inside a SQL literal
	stmt := "CREATE ROLE " + pgx.Identifier{user}.Sanitize() + " LOGIN PASSWORD '" + password + "'"
	for _, a := range o.Attributes {
		if !slices.Contains(attributes, a) {
			t.Fatalf("dbtest: unknown role attribute %q", a)
		}
		stmt += " " + a
	}
	if len(o.Roles) > 0 {
		quoted := make([]string, len(o.Roles))
		for i, r := range o.Roles {
			quoted[i] = pgx.Identifier{r}.Sanitize()
		}
		stmt += " IN ROLE " + strings.Join(quoted, ", ")
	}
	exec(ctx, t, conn, stmt)
	return user, password
}

func loginURL(host string, port uint16, user, password, dbName string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Path: "/" + dbName}
	p := strconv.Itoa(int(port))
	if strings.HasPrefix(host, "/") { // Unix socket directory
		u.RawQuery = url.Values{"host": {host}, "port": {p}}.Encode()
	} else {
		u.Host = net.JoinHostPort(host, p)
	}
	return u.String()
}

// newStamp returns "<unix seconds>_<random>", the creation stamp in
// database and login names.
func newStamp(t *testing.T) string {
	t.Helper()
	return strconv.FormatInt(time.Now().Unix(), 10) + "_" + randomHex(t, 4)
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// Eventually polls cond every 25ms until it reports true, failing the test
// when timeout passes first or cond returns an error. Tests wait for
// conditions with it instead of sleeping (CLAUDE.md, Test düzeni).
func Eventually(t *testing.T, timeout time.Duration, what string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		ok, err := cond()
		if err != nil {
			t.Fatalf("waiting for %s: %v", what, err)
		}
		if ok {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		case <-tick.C:
		case <-t.Context().Done():
			t.Fatalf("test ended while waiting for %s", what)
		}
	}
}
