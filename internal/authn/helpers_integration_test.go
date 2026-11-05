//go:build integration

package authn_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
)

var epoch = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

// Device ids of the tests: two browser profiles and a phone.
const (
	deviceW1    = "web-profile-0001"
	deviceW2    = "web-profile-0002"
	devicePhone = "phone-install-01"
)

// env is one test's database and services, on a fake clock.
type env struct {
	db        *db.DB
	maint     *db.DB
	q         *authndb.Queries
	clock     *clock.Fake
	reg       *prometheus.Registry
	passwords *countingHasher
	users     *authn.Service
	staff     *authn.StaffService
	revoked   *revocations
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := dbtest.New(t)
	database := d.Open(t, db.RoleApp)
	clk := clock.NewFake(epoch)
	logger := log.New(io.Discard, slog.LevelInfo)
	inner, err := authn.NewPasswords(authn.DefaultParams(), 4)
	if err != nil {
		t.Fatal(err)
	}
	passwords := &countingHasher{inner: inner}
	mp, err := authn.NewTokenClass(authn.AudienceMarketplace, tokenkeys.Generate("mp"), clk)
	if err != nil {
		t.Fatal(err)
	}
	st, err := authn.NewTokenClass(authn.AudienceStaff, tokenkeys.Generate("st"), clk)
	if err != nil {
		t.Fatal(err)
	}
	rev := &revocations{}
	reg := prometheus.NewRegistry()
	metrics, err := authn.NewMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	pepper := tokenkeys.GeneratePepper()
	users, err := authn.NewService(database, passwords, mp, pepper, clk, logger, metrics, rev.hook)
	if err != nil {
		t.Fatal(err)
	}
	staff, err := authn.NewStaffService(database, passwords, st, pepper, clk, logger, metrics)
	if err != nil {
		t.Fatal(err)
	}
	return &env{
		db: database, maint: d.Open(t, db.RoleMaint), q: authndb.New(database.Pool()), clock: clk, reg: reg,
		passwords: passwords, users: users, staff: staff, revoked: rev,
	}
}

// addUser creates an active user with password.
func (e *env) addUser(t *testing.T, email, password string) uuid.UUID {
	t.Helper()
	hash, err := e.passwords.inner.Hash(t.Context(), password)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	err = e.q.InsertUser(t.Context(), authndb.InsertUserParams{
		ID: id, Email: email, PasswordHash: &hash, Status: authn.UserActive, CreatedAt: e.clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// addStaff creates a staff member with the password opsPw.
func (e *env) addStaff(t *testing.T, email, status string, mustChange bool) uuid.UUID {
	t.Helper()
	hash, err := e.passwords.inner.Hash(t.Context(), opsPw)
	if err != nil {
		t.Fatal(err)
	}
	return e.addStaffHashed(t, email, status, mustChange, hash)
}

// addStaffHashed creates a staff member with a given password hash.
func (e *env) addStaffHashed(t *testing.T, email, status string, mustChange bool, hash string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	// Staff accounts are created by koala_maint (bootstrap, staff
	// management); the application role cannot insert them.
	_, err := e.maint.Pool().Exec(t.Context(), `
		INSERT INTO staff_members (id, email, password_hash, status, must_change_password, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, id, email, hash, status, mustChange, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) login(t *testing.T, email, password, device string) authn.TokenPair {
	t.Helper()
	pair, err := e.users.Login(t.Context(), authn.LoginInput{Email: email, Password: password, DeviceKey: device, Platform: "web"})
	if err != nil {
		t.Fatalf("Login(%s, %s) = %v", email, device, err)
	}
	return pair
}

// attempts counts the login_attempts rows of an address by outcome.
func (e *env) attempts(t *testing.T, kind, email string) authndb.CountLoginAttemptsRow {
	t.Helper()
	row, err := e.q.CountLoginAttempts(t.Context(), authndb.CountLoginAttemptsParams{PrincipalKind: kind, TargetHash: e.users.Target(email)})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// lockouts reads koala_auth_lockouts_total for a principal and scope.
func (e *env) lockouts(t *testing.T, principal, scope string) float64 {
	t.Helper()
	families, err := e.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != authn.MetricLockouts {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["principal"] == principal && labels["scope"] == scope {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no %s{principal=%q,scope=%q}", authn.MetricLockouts, principal, scope)
	return 0
}

func discard() *slog.Logger { return log.New(io.Discard, slog.LevelInfo) }

// wantErr checks an error's kind and code.
func wantErr(t *testing.T, err error, kind apperr.Kind, code apperr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("succeeded, want %s %s", kind, code)
	}
	if got := apperr.From(err); got.Kind != kind || got.Code != code {
		t.Fatalf("error = %v, want %s %s", err, kind, code)
	}
}

// countingHasher counts the argon2id work the services ask for:
// verifications (against a stored hash or the dummy) and new hashes.
type countingHasher struct {
	inner    *authn.Passwords
	verifies atomic.Int64 // Verify and VerifyDummy
	dummies  atomic.Int64 // VerifyDummy only
	hashes   atomic.Int64
}

func (c *countingHasher) Hash(ctx context.Context, p string) (string, error) {
	c.hashes.Add(1)
	return c.inner.Hash(ctx, p)
}

func (c *countingHasher) Verify(ctx context.Context, stored, p string) (bool, bool, error) {
	c.verifies.Add(1)
	return c.inner.Verify(ctx, stored, p)
}

func (c *countingHasher) VerifyDummy(ctx context.Context, p string) error {
	c.verifies.Add(1)
	c.dummies.Add(1)
	return c.inner.VerifyDummy(ctx, p)
}

// cost is the argon2id work of one call: verifications against a stored
// hash, against the dummy, and new hashes.
type cost struct{ verifies, dummies, hashes int64 }

// costOf runs f and returns the work it asked for.
func (c *countingHasher) costOf(f func()) cost {
	v, d, h := c.verifies.Load(), c.dummies.Load(), c.hashes.Load()
	f()
	return cost{
		verifies: c.verifies.Load() - v - (c.dummies.Load() - d),
		dummies:  c.dummies.Load() - d,
		hashes:   c.hashes.Load() - h,
	}
}

// revocations records the sessions the revoke hook was told about.
type revocations struct {
	mu  sync.Mutex
	ids []uuid.UUID
}

func (r *revocations) hook(_ context.Context, _, sessionID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, sessionID)
}

func (r *revocations) has(id uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.ids {
		if x == id {
			return true
		}
	}
	return false
}
