//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/softdelete/softdeletetest"
)

// TestReadTestOnTheContract is the generated read test (ADR-0020 §11) on
// the real contract and router: every GET's x-resource that names a
// soft-deleted table (users, staff_members) gets a fixture; user_sessions,
// realtime_tickets and staff_role_assignments are exempt and have nothing
// to hide. /v1/me and /v1/admin/me read the caller itself
// (x-resource-self): after the soft delete the caller no longer
// authenticates.
func TestReadTestOnTheContract(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	userHash, err := a.passwords.Hash(t.Context(), userPw)
	if err != nil {
		t.Fatal(err)
	}
	staffHash, err := a.passwords.Hash(t.Context(), staffPw)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	emails := map[uuid.UUID]string{}
	tokens := map[uuid.UUID]string{}
	fixtures := map[string]softdeletetest.Fixture{
		"users": func(ctx context.Context, q db.Querier) (uuid.UUID, error) {
			id := uuid.Must(uuid.NewV7())
			email := "read-" + id.String() + "@example.com"
			err := authndb.New(q).InsertUser(ctx, authndb.InsertUserParams{
				ID: id, Email: email, PasswordHash: &userHash, Status: authn.UserActive, CreatedAt: a.clock.Now(),
			})
			if err != nil {
				return uuid.Nil, fmt.Errorf("insert user: %w", err)
			}
			mu.Lock()
			emails[id] = email
			mu.Unlock()
			return id, nil
		},
		// The application cannot create staff (ADR-0025): the fixture
		// writes as koala_maint.
		"staff_members": func(ctx context.Context, _ db.Querier) (uuid.UUID, error) {
			id := uuid.Must(uuid.NewV7())
			email := "read-" + id.String() + "@iskoala.com"
			_, err := a.maint.Pool().Exec(ctx, `
				INSERT INTO staff_members (id, email, password_hash, status, must_change_password, created_at)
				VALUES ($1, $2, $3, 'ACTIVE', false, $4)`, id, email, staffHash, a.clock.Now())
			if err != nil {
				return uuid.Nil, fmt.Errorf("insert staff member: %w", err)
			}
			mu.Lock()
			emails[id] = email
			mu.Unlock()
			return id, nil
		},
	}
	authorize := func(ctx context.Context, table string, id uuid.UUID, r *http.Request) error {
		mu.Lock()
		defer mu.Unlock()
		if table == "users" {
			setClient(r.Header)
		}
		tok, ok := tokens[id]
		if !ok {
			switch table {
			case "users":
				pair, err := a.mods.users.Login(ctx, authn.LoginInput{
					Email: emails[id], Password: userPw, DeviceKey: testDevice, Platform: string(httpx.PlatformIOS),
				})
				if err != nil {
					return fmt.Errorf("sign in user: %w", err)
				}
				tok = pair.AccessToken
			case "staff_members":
				pair, err := a.mods.staff.Login(ctx, emails[id], staffPw, authn.ClientKey(netip.MustParseAddr("192.0.2.1")))
				if err != nil {
					return fmt.Errorf("sign in staff: %w", err)
				}
				tok = pair.AccessToken
			default:
				return fmt.Errorf("no caller for %s", table)
			}
			tokens[id] = tok
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		return nil
	}

	softdeletetest.ReadTest{
		Spec: spec, Handler: a.router, App: a.db, Catalog: dbtest.SoftDeleteCatalog(t),
		Fixtures: fixtures, Authorize: authorize,
	}.Run(t)
}
