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
	"github.com/is-koala/koala-api/internal/media"
	"github.com/is-koala/koala-api/internal/media/mediadb"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/softdelete/softdeletetest"
)

// TestReadTestOnTheContract is the generated read test (ADR-0020 §11) on
// the real contract and router: every GET's x-resource that names a
// soft-deleted table (users, staff_members, media, educations) gets a fixture; user_sessions,
// realtime_tickets, staff_role_assignments and user_verifications are
// exempt and have nothing to hide. /v1/me and /v1/admin/me read the caller itself
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

	// Education records point at reference rows (M4), which the
	// application cannot write.
	ref := a.seedReference(t)

	var mu sync.Mutex
	emails := map[uuid.UUID]string{}
	tokens := map[uuid.UUID]string{}
	// owners maps a media or education fixture to its owner: getMedia and
	// listMyEducations read the caller's own rows, so the owner is the
	// caller that may read them.
	owners := map[uuid.UUID]uuid.UUID{}
	insertUser := func(ctx context.Context, q db.Querier) (uuid.UUID, error) {
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
	}
	fixtures := map[string]softdeletetest.Fixture{
		// A PENDING upload of a fresh user, as startMediaUpload leaves it.
		"media": func(ctx context.Context, q db.Querier) (uuid.UUID, error) {
			owner, err := insertUser(ctx, q)
			if err != nil {
				return uuid.Nil, err
			}
			id := uuid.Must(uuid.NewV7())
			_, err = mediadb.New(q).InsertMedia(ctx, mediadb.InsertMediaParams{
				ID: id, OwnerID: owner, Kind: string(media.KindProfilePicture),
				UploadKey: "incoming/" + id.String() + ".jpg", DeclaredSize: 1024, CreatedAt: a.clock.Now(),
			})
			if err != nil {
				return uuid.Nil, fmt.Errorf("insert media: %w", err)
			}
			mu.Lock()
			owners[id] = owner
			mu.Unlock()
			return id, nil
		},
		// An education record of a fresh user (ADR-0036).
		"educations": func(ctx context.Context, q db.Querier) (uuid.UUID, error) {
			owner, err := insertUser(ctx, q)
			if err != nil {
				return uuid.Nil, err
			}
			id := uuid.Must(uuid.NewV7())
			_, err = q.Exec(ctx, `
				INSERT INTO active_educations (id, user_id, university_id, degree, start_year, created_at)
				VALUES ($1, $2, $3, 'BACHELOR', 2014, $4)`, id, owner, ref.university, a.clock.Now())
			if err != nil {
				return uuid.Nil, fmt.Errorf("insert education: %w", err)
			}
			mu.Lock()
			owners[id] = owner
			mu.Unlock()
			return id, nil
		},
		"users": insertUser,
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
		if table == "media" || table == "educations" {
			// The owner reads it, as a user.
			id, table = owners[id], "users"
		}
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
