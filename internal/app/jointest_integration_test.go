//go:build integration

package app

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/netip"
	"regexp"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/api"
	dbsrc "github.com/is-koala/koala-api/db"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/platform/softdelete/softdeletetest"
)

// queryName starts one sqlc query.
var queryName = regexp.MustCompile(`(?m)^-- name: (\w+)`)

// markedParents reads the inner-by-design markers (ADR-0027 §5) of every
// sqlc query and returns the detach parents they exempt. It is what ties
// the marker in the SQL to the JoinTest declaration below: the same
// decision is written in two places, and neither may be removed alone.
func markedParents(t *testing.T) map[string]bool {
	t.Helper()
	queries := dbsrc.Queries()
	files, err := fs.Glob(queries, "*/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no query files under db/queries")
	}
	out := map[string]bool{}
	for _, f := range files {
		data, err := fs.ReadFile(queries, f)
		if err != nil {
			t.Fatal(err)
		}
		bounds := queryName.FindAllStringIndex(string(data), -1)
		for i, b := range bounds {
			end := len(data)
			if i+1 < len(bounds) {
				end = bounds[i+1][0]
			}
			for _, parent := range softdelete.MarkedParents(string(data[b[0]:end])) {
				out[parent] = true
			}
		}
	}
	return out
}

// TestJoinTestOnTheContract is the generated JOIN test (ADR-0020 Karar 11,
// ADR-0027 §2) on the real contract and router: every detach edge of
// db/softdelete.yaml says what the child's GET operations do once the
// parent is soft-deleted, and the test holds the answer to it. A detach
// edge added to the catalog has no case and fails here.
//
// Both edges the contract reads today hide the child on purpose, each for
// its own reason: a session's user is the caller, so its deletion ends the
// session (401), and a session whose device is gone is dead and has no
// platform to report, so the session endpoints leave it out. The positive
// side of the rule — the child stays with its parent fields null — is
// proved on the example resources
// (softdeletetest.TestJoinTestOnExampleResources), because no real
// endpoint reads a detach child whose parent is neither the caller nor its
// device yet.
func TestJoinTestOnTheContract(t *testing.T) {
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

	// addUser writes one active user and returns its id and address.
	addUser := func(ctx context.Context) (uuid.UUID, string, error) {
		id := uuid.Must(uuid.NewV7())
		email := "join-" + id.String() + "@example.com"
		err := a.q.InsertUser(ctx, authndb.InsertUserParams{
			ID: id, Email: email, PasswordHash: &userHash, Status: authn.UserActive, CreatedAt: a.clock.Now(),
		})
		if err != nil {
			return uuid.Nil, "", fmt.Errorf("insert user: %w", err)
		}
		return id, email, nil
	}
	signIn := func(ctx context.Context, email, deviceKey string) (authn.TokenPair, error) {
		pair, err := a.mods.users.Login(ctx, authn.LoginInput{
			Email: email, Password: userPw, DeviceKey: deviceKey, Platform: string(httpx.PlatformIOS),
		})
		if err != nil {
			return authn.TokenPair{}, fmt.Errorf("sign in %s on %s: %w", email, deviceKey, err)
		}
		return pair, nil
	}
	// bearer answers every request of one case as the caller the fixture
	// signed in. The fixture and the requests of a case run in the same
	// subtest goroutine, one after the other.
	bearer := func(token *string, client bool) func(context.Context, softdeletetest.Row, *http.Request) error {
		return func(_ context.Context, _ softdeletetest.Row, r *http.Request) error {
			if client {
				setClient(r.Header)
			}
			r.Header.Set("Authorization", "Bearer "+*token)
			return nil
		}
	}

	var ownToken, otherToken, staffToken string
	cases := map[string]softdeletetest.DetachCase{
		// The session's own user: deleting it ends the session.
		"user_sessions.user_id": {
			Fixture: func(ctx context.Context) (softdeletetest.Row, error) {
				user, email, err := addUser(ctx)
				if err != nil {
					return softdeletetest.Row{}, err
				}
				pair, err := signIn(ctx, email, testDevice)
				if err != nil {
					return softdeletetest.Row{}, err
				}
				ownToken = pair.AccessToken
				return softdeletetest.Row{Child: pair.SessionID, Parent: user}, nil
			},
			Authorize: bearer(&ownToken, true),
			Outcome:   softdeletetest.Unauthenticated,
			Reason: "the session's user is the caller: the per-request check reaches the user by LEFT JOIN and " +
				"requires it active, so a deleted user authenticates nothing (ADR-0025 §2)",
		},
		// Another device of the same user: the caller survives, the
		// session of the deleted device drops out.
		"user_sessions.device_id": {
			Fixture: func(ctx context.Context) (softdeletetest.Row, error) {
				user, email, err := addUser(ctx)
				if err != nil {
					return softdeletetest.Row{}, err
				}
				caller, err := signIn(ctx, email, testDevice)
				if err != nil {
					return softdeletetest.Row{}, err
				}
				otherToken = caller.AccessToken
				key := "join-" + uuid.NewString()
				other, err := signIn(ctx, email, key)
				if err != nil {
					return softdeletetest.Row{}, err
				}
				var device uuid.UUID
				err = a.db.Pool().QueryRow(ctx,
					"SELECT id FROM active_devices WHERE user_id = $1 AND device_key = $2", user, key).Scan(&device)
				if err != nil {
					return softdeletetest.Row{}, fmt.Errorf("load the second device: %w", err)
				}
				return softdeletetest.Row{Child: other.SessionID, Parent: device}, nil
			},
			Authorize: bearer(&otherToken, true),
			Outcome:   softdeletetest.Hides,
			Reason: "a session whose device is soft-deleted is dead and Session.platform, which comes from the " +
				"device, is required; the list and the detail leave it out (AUTH-016, db/queries/authn/sessions.sql)",
		},
		// The assignment's staff member is the caller.
		"staff_role_assignments.staff_id": {
			Fixture: func(ctx context.Context) (softdeletetest.Row, error) {
				id := uuid.Must(uuid.NewV7())
				email := "join-" + id.String() + "@iskoala.com"
				// The application cannot create staff (ADR-0025).
				_, err := a.maint.Pool().Exec(ctx, `
					INSERT INTO staff_members (id, email, password_hash, status, must_change_password, created_at)
					VALUES ($1, $2, $3, 'ACTIVE', false, $4)`, id, email, staffHash, a.clock.Now())
				if err != nil {
					return softdeletetest.Row{}, fmt.Errorf("insert staff member: %w", err)
				}
				_, err = a.maint.Pool().Exec(ctx, `
					INSERT INTO staff_role_assignments (staff_id, role_id, created_at)
					SELECT $1, r.id, $2 FROM staff_roles r WHERE r.code = 'CATALOG_EDITOR'`, id, a.clock.Now())
				if err != nil {
					return softdeletetest.Row{}, fmt.Errorf("assign CATALOG_EDITOR: %w", err)
				}
				pair, err := a.mods.staff.Login(ctx, email, staffPw, authn.ClientKey(netip.MustParseAddr("192.0.2.1")))
				if err != nil {
					return softdeletetest.Row{}, fmt.Errorf("sign in staff: %w", err)
				}
				staffToken = pair.AccessToken
				// The assignment has no id of its own; the permission it
				// grants proves the answer carries it.
				return softdeletetest.Row{Parent: id, Shown: "category.manage"}, nil
			},
			Authorize: bearer(&staffToken, false),
			Outcome:   softdeletetest.Unauthenticated,
			Reason: "the assignment's staff member is the caller: a deleted member reaches no role at all " +
				"(db/queries/authz/staff.sql) and does not authenticate (AUTH-012)",
		},
		"staff_sessions.staff_id": {
			Unread: "no GET operation reads staff_sessions: a staff session is checked on every request and never listed (ADR-0025 §6)",
		},
		"staff_login_sources.staff_id": {
			Unread: "no GET operation reads staff_login_sources: it is the sign-in failure counter of one client address, read only by the sign-in path (ADR-0025 §5)",
		},
		"example_comments.author_id": {
			Unread: "an example table of the test databases (dbtest/testdata); the real contract has no operation on it, softdeletetest's own test covers the edge",
		},
	}

	softdeletetest.JoinTest{
		Spec: spec, Handler: a.router, App: a.db,
		Catalog: dbtest.SoftDeleteCatalog(t), Cases: cases,
		Marked: markedParents(t),
	}.Run(t)
}
