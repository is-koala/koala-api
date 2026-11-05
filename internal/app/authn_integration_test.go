//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/realtime"
)

const (
	testDevice = "app-test-device-01"
	userEmail  = "ayse@example.com"
	userPw     = "ayşe'nin uzun parolası"
	staffEmail = "ops@iskoala.com"
	staffPw    = "personelin uzun parolası"
)

// authApp is the wired router on a real database and a fake clock.
type authApp struct {
	router    http.Handler
	clock     *clock.Fake
	q         *authndb.Queries
	passwords *authn.Passwords
}

func newAuthApp(t *testing.T, groups map[string]httpx.GroupLimits) *authApp {
	t.Helper()
	database := dbtest.New(t).Open(t, db.RoleApp)
	clk := clock.NewFake(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	mods := testModulesOn(t, database, clk)
	router, err := newRouter(context.Background(), config.Config{Env: config.EnvTest}, logger,
		httpx.NewHealth(logger), prometheus.NewRegistry(), groups, mods)
	if err != nil {
		t.Fatal(err)
	}
	passwords, err := authn.NewPasswords(authn.DefaultParams(), 2)
	if err != nil {
		t.Fatal(err)
	}
	return &authApp{router: router, clock: clk, q: authndb.New(database.Pool()), passwords: passwords}
}

func (a *authApp) addUser(t *testing.T) {
	t.Helper()
	hash, err := a.passwords.Hash(t.Context(), userPw)
	if err != nil {
		t.Fatal(err)
	}
	err = a.q.InsertUser(t.Context(), authndb.InsertUserParams{
		ID: uuid.Must(uuid.NewV7()), Email: userEmail, PasswordHash: &hash, Status: authn.UserActive, CreatedAt: a.clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (a *authApp) addStaff(t *testing.T, mustChange bool) {
	t.Helper()
	hash, err := a.passwords.Hash(t.Context(), staffPw)
	if err != nil {
		t.Fatal(err)
	}
	err = a.q.InsertStaffMember(t.Context(), authndb.InsertStaffMemberParams{
		ID: uuid.Must(uuid.NewV7()), Email: staffEmail, PasswordHash: hash, Status: authn.StaffActive,
		MustChangePassword: mustChange, CreatedAt: a.clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// call sends a request. A marketplace path carries a current iOS build's
// headers; an admin path carries none (the panel sends no device).
func (a *authApp) call(t *testing.T, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader = http.NoBody
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(httpx.HeaderRequestID, "req-auth")
	if !strings.HasPrefix(path, httpx.AdminPathPrefix) {
		setClient(req.Header)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	return rec
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if code != "" && !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("body = %s, want code %s", rec.Body, code)
	}
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

type tokens struct {
	AccessToken        string `json:"accessToken"`
	RefreshToken       string `json:"refreshToken"`
	MustChangePassword bool   `json:"mustChangePassword"`
}

func (a *authApp) loginUser(t *testing.T) tokens {
	t.Helper()
	rec := a.call(t, http.MethodPost, "/v1/auth/login", "", `{"email":"`+userEmail+`","password":"`+userPw+`"}`)
	wantStatus(t, rec, http.StatusOK, "")
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("token response Cache-Control = %q, want no-store (RFC 6749 §5.1)", cc)
	}
	return decodeBody[tokens](t, rec)
}

func (a *authApp) loginStaff(t *testing.T, password string) tokens {
	t.Helper()
	rec := a.call(t, http.MethodPost, "/v1/admin/auth/login", "", `{"email":"`+staffEmail+`","password":"`+password+`"}`)
	wantStatus(t, rec, http.StatusOK, "")
	return decodeBody[tokens](t, rec)
}

// AUTH-001: the wrong-password and unknown-address answers are the same
// bytes (same request id, so even that field matches).
func TestLoginFailureBodiesAreIdentical(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	a.addUser(t)

	known := a.call(t, http.MethodPost, "/v1/auth/login", "", `{"email":"`+userEmail+`","password":"yanlış ama uzun parola"}`)
	unknown := a.call(t, http.MethodPost, "/v1/auth/login", "", `{"email":"kimse@example.com","password":"yanlış ama uzun parola"}`)
	wantStatus(t, known, http.StatusUnauthorized, string(authn.CodeEmailOrPasswordMismatch))
	if !bytes.Equal(known.Body.Bytes(), unknown.Body.Bytes()) {
		t.Errorf("AUTH-001: bodies differ:\n%s\n%s", known.Body, unknown.Body)
	}
	if known.Code != unknown.Code {
		t.Errorf("AUTH-001: status %d vs %d", known.Code, unknown.Code)
	}
	a.loginUser(t)
}

// AUTH-015 (D5): the marketplace and staff identities do not cross; an
// anonymous request does not reach the staff surface, and a staff path
// the contract does not map is refused, not guessed at. AUTH-013: an
// anonymous caller cannot log anyone out.
func TestTokenClassesOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	a.addUser(t)
	a.addStaff(t, false)
	user, staff := a.loginUser(t), a.loginStaff(t, staffPw)

	wantStatus(t, a.call(t, http.MethodGet, "/v1/me", user.AccessToken, ""), http.StatusOK, "")
	wantStatus(t, a.call(t, http.MethodGet, "/v1/admin/me", staff.AccessToken, ""), http.StatusOK, "")

	t.Run("AUTH-015/marketplace token on the staff surface", func(t *testing.T) {
		t.Parallel()
		wantStatus(t, a.call(t, http.MethodGet, "/v1/admin/me", user.AccessToken, ""), http.StatusUnauthorized, string(authn.CodeTokenInvalid))
	})
	t.Run("AUTH-015/staff token on the marketplace", func(t *testing.T) {
		t.Parallel()
		wantStatus(t, a.call(t, http.MethodGet, "/v1/me", staff.AccessToken, ""), http.StatusUnauthorized, string(authn.CodeTokenInvalid))
	})
	t.Run("AUTH-015/anonymous on the staff surface", func(t *testing.T) {
		t.Parallel()
		rec := a.call(t, http.MethodGet, "/v1/admin/me", "", "")
		wantStatus(t, rec, http.StatusUnauthorized, string(authn.CodeTokenMissing))
		if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
		}
	})
	t.Run("AUTH-015/unmapped staff path", func(t *testing.T) {
		t.Parallel()
		wantStatus(t, a.call(t, http.MethodGet, "/v1/admin/payouts", staff.AccessToken, ""), http.StatusNotFound, "ROUTE_NOT_FOUND")
	})
	t.Run("AUTH-013/anonymous logout", func(t *testing.T) {
		t.Parallel()
		wantStatus(t, a.call(t, http.MethodPost, "/v1/admin/auth/logout", "", ""), http.StatusUnauthorized, string(authn.CodeTokenMissing))
	})
	t.Run("AUTH-012/refresh token is not an access token", func(t *testing.T) {
		t.Parallel()
		wantStatus(t, a.call(t, http.MethodGet, "/v1/admin/me", staff.RefreshToken, ""), http.StatusUnauthorized, string(authn.CodeTokenInvalid))
	})
}

// AUTH-014: a member who must change the password reaches only the change
// (and logout); after the change, a new sign-in reaches everything.
func TestStaffMustChangePassword(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	a.addStaff(t, true)

	first := a.loginStaff(t, staffPw)
	if !first.MustChangePassword {
		t.Fatal("AUTH-014: new account signed in without mustChangePassword")
	}
	wantStatus(t, a.call(t, http.MethodGet, "/v1/admin/me", first.AccessToken, ""),
		http.StatusForbidden, string(authn.CodeStaffPasswordChangeRequired))
	wantStatus(t, a.call(t, http.MethodPost, "/v1/admin/auth/password", first.AccessToken,
		`{"currentPassword":"`+staffPw+`","newPassword":"yepyeni uzun bir parola"}`), http.StatusNoContent, "")
	wantStatus(t, a.call(t, http.MethodGet, "/v1/admin/me", first.AccessToken, ""),
		http.StatusUnauthorized, string(authn.CodeStaffSessionEnded))

	second := a.loginStaff(t, "yepyeni uzun bir parola")
	rec := a.call(t, http.MethodGet, "/v1/admin/me", second.AccessToken, "")
	wantStatus(t, rec, http.StatusOK, "")
	if me := decodeBody[tokens](t, rec); me.MustChangePassword {
		t.Error("AUTH-014: flag still set")
	}
}

// AUTH-017: an open session gets a non-cacheable, short-lived WebSocket
// ticket. The ticket authenticates no REST request; it opens the
// WebSocket once; after the session is closed, or after 60 seconds, it
// opens nothing. An access token is no ticket.
func TestRealtimeTicketsOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	a.addUser(t)
	srv := httptest.NewServer(a.router)
	t.Cleanup(srv.Close)

	issue := func(t *testing.T, access string) string {
		t.Helper()
		rec := a.call(t, http.MethodPost, realtime.PathTickets, access, "")
		wantStatus(t, rec, http.StatusCreated, "")
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Error("AUTH-017: ticket response is cacheable")
		}
		return decodeBody[struct {
			Ticket string `json:"ticket"`
		}](t, rec).Ticket
	}
	dial := func(t *testing.T, ticket string) (*websocket.Conn, *http.Response, error) {
		t.Helper()
		h := http.Header{}
		setClient(h)
		h.Set(httpx.HeaderDeviceID, testDevice)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		return websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+realtime.PathConnect, &websocket.DialOptions{
			Subprotocols: []string{realtime.Subprotocol, realtime.TicketProtocolPrefix + ticket},
			HTTPHeader:   h,
		})
	}
	refused := func(t *testing.T, ticket string) {
		t.Helper()
		c, resp, err := dial(t, ticket)
		if err == nil {
			_ = c.CloseNow()
			t.Fatal("AUTH-017: connection opened")
		}
		if resp == nil {
			t.Fatalf("AUTH-017: no handshake response: %v", err)
		}
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("AUTH-017: handshake status %d, want 401", resp.StatusCode)
		}
	}

	user := a.loginUser(t)
	ticket := issue(t, user.AccessToken)
	wantStatus(t, a.call(t, http.MethodGet, "/v1/me", ticket, ""), http.StatusUnauthorized, string(authn.CodeTokenInvalid))

	c, resp, err := dial(t, ticket)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close() // the library has replaced it with an empty body
	}
	if err != nil {
		t.Fatalf("AUTH-017: dial with a fresh ticket: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	refused(t, ticket) // single use

	refused(t, user.AccessToken) // an access token is no ticket

	stale := issue(t, user.AccessToken)
	a.clock.Advance(realtime.TicketTTL)
	refused(t, stale)

	revoked := issue(t, user.AccessToken)
	wantStatus(t, a.call(t, http.MethodPost, "/v1/auth/logout", user.AccessToken, ""), http.StatusNoContent, "")
	refused(t, revoked)
}

// AUTH-018: sign-in has its own budget per target, so one account being
// hammered behind an office NAT does not lock out the next. Staff
// sign-in, refresh and the staff password change share the auth group.
// An oversized auth body is refused before it is buffered.
func TestAuthRateLimits(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, rateLimitGroups())
	a.addUser(t)

	burst := rateLimitGroups()[groupAuth].PerCustom.Burst
	var last *httptest.ResponseRecorder
	for range burst + 1 {
		last = a.call(t, http.MethodPost, "/v1/auth/login", "", `{"email":"saldirilan@example.com","password":"yanlış ama uzun parola"}`)
	}
	wantStatus(t, last, http.StatusTooManyRequests, httpx.CodeRateLimited)
	if last.Header().Get("Retry-After") == "" {
		t.Error("AUTH-018: 429 without Retry-After")
	}
	// Same address, another target: not limited.
	a.loginUser(t)

	spec, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	routes, err := httpx.RateLimitRoutes(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{
		"POST /v1/auth/login", "POST /v1/auth/refresh", "POST /v1/admin/auth/login",
		"POST /v1/admin/auth/refresh", "POST /v1/admin/auth/password",
	} {
		if routes[r] != groupAuth {
			t.Errorf("AUTH-018: %s is in group %q, want %s", r, routes[r], groupAuth)
		}
	}

	// A chunked body over the operation's limit: 413 before buffering.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/auth/login",
		io.MultiReader(strings.NewReader(`{"email":"x@example.com","password":"`), strings.NewReader(strings.Repeat("a", 64<<10)), strings.NewReader(`"}`)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	setClient(req.Header)
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	wantStatus(t, rec, http.StatusRequestEntityTooLarge, "REQUEST_BODY_TOO_LARGE")
}
