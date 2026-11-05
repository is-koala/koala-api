//go:build integration

package authn_test

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

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
	q         *authndb.Queries
	clock     *clock.Fake
	passwords *countingHasher
	users     *authn.Service
	staff     *authn.StaffService
	revoked   *revocations
}

func newEnv(t *testing.T) *env {
	t.Helper()
	database := dbtest.New(t).Open(t, db.RoleApp)
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
	users, err := authn.NewService(database, passwords, mp, clk, logger, rev.hook)
	if err != nil {
		t.Fatal(err)
	}
	staff, err := authn.NewStaffService(database, passwords, st, clk, logger)
	if err != nil {
		t.Fatal(err)
	}
	return &env{
		db: database, q: authndb.New(database.Pool()), clock: clk, passwords: passwords,
		users: users, staff: staff, revoked: rev,
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
	id := uuid.Must(uuid.NewV7())
	err = e.q.InsertStaffMember(t.Context(), authndb.InsertStaffMemberParams{
		ID: id, Email: email, PasswordHash: hash, Status: status, MustChangePassword: mustChange, CreatedAt: e.clock.Now(),
	})
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

// attempts counts the login_attempts rows of an address.
func (e *env) attempts(t *testing.T, kind, email string) (succeeded, failed int64) {
	t.Helper()
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	row, err := e.q.CountLoginAttempts(t.Context(), authndb.CountLoginAttemptsParams{PrincipalKind: kind, TargetHash: h[:]})
	if err != nil {
		t.Fatal(err)
	}
	return row.Succeeded, row.Failed
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

// countingHasher counts the argon2id verifications the services ask for.
type countingHasher struct {
	inner    *authn.Passwords
	verifies atomic.Int64
}

func (c *countingHasher) Hash(ctx context.Context, p string) (string, error) {
	return c.inner.Hash(ctx, p)
}

func (c *countingHasher) Verify(ctx context.Context, stored, p string) (bool, bool, error) {
	c.verifies.Add(1)
	return c.inner.Verify(ctx, stored, p)
}

func (c *countingHasher) VerifyDummy(ctx context.Context, p string) error {
	c.verifies.Add(1)
	return c.inner.VerifyDummy(ctx, p)
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
