//go:build integration

package authn_test

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/realtime"
)

const (
	alice    = "alice@example.com"
	alicePw  = "alice'in uzun parolası"
	bob      = "bob@example.com"
	bobPw    = "bob'un da uzun parolası"
	nobody   = "nobody@example.com"
	wrongPw  = "tamamen yanlış bir parola"
	oldShort = "kisa8chr" // an 8-character password set under an older rule
)

// AUTH-001: an unregistered address and a registered one with a wrong
// password get the same error, both pay one argon2id verification, and
// both are logged as failed attempts. The HTTP bodies are compared byte for
// byte in internal/app (TestLoginFailureBodiesAreIdentical).
func TestLoginConcealsUnknownAddress(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addUser(t, alice, alicePw)

	before := e.passwords.verifies.Load()
	_, errKnown := e.users.Login(t.Context(), authn.LoginInput{Email: alice, Password: wrongPw, DeviceKey: deviceW1})
	afterKnown := e.passwords.verifies.Load()
	_, errUnknown := e.users.Login(t.Context(), authn.LoginInput{Email: nobody, Password: wrongPw, DeviceKey: deviceW1})
	afterUnknown := e.passwords.verifies.Load()

	t.Run("AUTH-001/same error", func(t *testing.T) {
		t.Parallel()
		wantErr(t, errKnown, apperr.KindUnauthenticated, authn.CodeEmailOrPasswordMismatch)
		wantErr(t, errUnknown, apperr.KindUnauthenticated, authn.CodeEmailOrPasswordMismatch)
		if errKnown.Error() != errUnknown.Error() {
			t.Errorf("errors differ: %q vs %q", errKnown, errUnknown)
		}
	})
	t.Run("AUTH-001/same verification cost", func(t *testing.T) {
		t.Parallel()
		if afterKnown-before != 1 || afterUnknown-afterKnown != 1 {
			t.Errorf("verifications: known %d, unknown %d; want 1 each", afterKnown-before, afterUnknown-afterKnown)
		}
	})
	t.Run("AUTH-001/both recorded as failed", func(t *testing.T) {
		t.Parallel()
		for _, addr := range []string{alice, nobody} {
			if row := e.attempts(t, "USER", addr); row.Succeeded != 0 || row.Failed != 1 {
				t.Errorf("AUTH-001 %s attempts: %+v; want one failure", addr, row)
			}
		}
	})
}

// AUTH-009: sign-in accepts whatever password the account already has,
// even one the current rule would refuse.
func TestLoginAcceptsAnExistingShortPassword(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addUser(t, alice, oldShort)
	if _, err := e.users.Login(t.Context(), authn.LoginInput{Email: alice, Password: oldShort, DeviceKey: deviceW1}); err != nil {
		t.Fatalf("AUTH-009: sign-in with an existing 8-character password: %v", err)
	}
}

// AUTH-002: a new sign-in on a device closes that device's session; other
// devices keep theirs. Without a device id nothing is looked up.
func TestLoginReplacesTheDeviceSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addUser(t, alice, alicePw)

	s1 := e.login(t, alice, alicePw, deviceW1)
	s2 := e.login(t, alice, alicePw, deviceW1)
	s3 := e.login(t, alice, alicePw, deviceW2)

	t.Run("AUTH-002/same device replaces", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		_, err := e.users.Authenticate(ctx, s1.AccessToken, deviceW1)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionInactive)
		if !e.revoked.has(s1.SessionID) {
			t.Error("the replaced session's connections were not told to close")
		}
		if _, err := e.users.Authenticate(ctx, s2.AccessToken, deviceW1); err != nil {
			t.Errorf("new session: %v", err)
		}
	})
	t.Run("AUTH-002/other device independent", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		if _, err := e.users.Authenticate(ctx, s3.AccessToken, deviceW2); err != nil {
			t.Errorf("W2 session: %v", err)
		}
		if _, err := e.users.Authenticate(ctx, s2.AccessToken, deviceW1); err != nil {
			t.Errorf("W1 session after the W2 sign-in: %v", err)
		}
	})
	t.Run("AUTH-002/no device id, no lookup", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		for _, device := range []string{"", "short", "has space in it"} {
			before := e.passwords.verifies.Load()
			_, err := e.users.Login(ctx, authn.LoginInput{Email: bob, Password: bobPw, DeviceKey: device})
			wantErr(t, err, apperr.KindInvalid, "REQUEST_INVALID")
			if got := e.passwords.verifies.Load() - before; got != 0 {
				t.Errorf("device %q: %d verifications; credentials were looked at", device, got)
			}
		}
		if row := e.attempts(t, "USER", bob); row.Succeeded+row.Failed+row.Locked != 0 {
			t.Errorf("AUTH-002: attempts recorded for a request without a device: %+v", row)
		}
	})
}

// AUTH-003 (K12): the session comes from the refresh token, so an access
// token expired for days is not needed; forged or foreign credentials
// cannot refresh.
func TestRefreshNeedsNoAccessToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addUser(t, alice, alicePw)
	ctx := t.Context()
	pair := e.login(t, alice, alicePw, deviceW1)

	user, err := e.users.Authenticate(ctx, pair.AccessToken, deviceW1)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := realtime.NewTickets(realtime.NewPostgresTickets(e.db.Pool()), alwaysActive{}, e.clock, discard()).
		Issue(ctx, realtime.Principal{UserID: realtime.UserID(user.UserID.String()), SessionID: pair.SessionID.String(), DeviceID: deviceW1})
	if err != nil {
		t.Fatal(err)
	}

	e.clock.Advance(3 * 24 * time.Hour)
	t.Run("AUTH-003/strict check refuses the expired access token", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		_, err := e.users.Authenticate(ctx, pair.AccessToken, deviceW1)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeTokenExpired)
	})
	t.Run("AUTH-003/forged and foreign credentials are refused", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		for name, v := range map[string]string{
			"forged":          flip(pair.RefreshToken),
			"malformed":       "not-a-token",
			"access token":    pair.AccessToken,
			"realtime ticket": ticket.Value,
		} {
			_, err := e.users.Refresh(ctx, v, deviceW1)
			if apperr.From(err).Code != authn.CodeRefreshTokenInvalid {
				t.Errorf("%s: Refresh() = %v, want %s", name, err, authn.CodeRefreshTokenInvalid)
			}
		}
	})
	t.Run("AUTH-003/refresh three days after expiry", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		next, err := e.users.Refresh(ctx, pair.RefreshToken, deviceW1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.users.Authenticate(ctx, next.AccessToken, deviceW1); err != nil {
			t.Fatalf("new access token: %v", err)
		}
	})
}

// AUTH-004: a refresh from another device closes the session; the owner's
// refresh then fails too. A request from another device does the same.
func TestAnotherDeviceRevokesTheSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addUser(t, alice, alicePw)

	t.Run("AUTH-004/refresh", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		pair := e.login(t, alice, alicePw, deviceW1)
		_, err := e.users.Refresh(ctx, pair.RefreshToken, deviceW2)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionDeviceMismatch)
		_, err = e.users.Refresh(ctx, pair.RefreshToken, deviceW1)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionInactive)
		if !e.revoked.has(pair.SessionID) {
			t.Error("revocation not announced")
		}
	})
	t.Run("AUTH-004/request", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		pair := e.login(t, alice, alicePw, devicePhone)
		_, err := e.users.Authenticate(ctx, pair.AccessToken, deviceW2)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionDeviceMismatch)
		_, err = e.users.Authenticate(ctx, pair.AccessToken, devicePhone)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionInactive)
	})
}

// AUTH-005 (K12 target): every refresh rotates the token; a rotated token
// presented again revokes the whole family; an unknown token changes
// nothing; a closed or expired session cannot refresh. The idle limit
// slides, the absolute one does not.
func TestRefreshRotation(t *testing.T) {
	t.Parallel()

	t.Run("AUTH-005/rotation and reuse detection", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		e := newEnv(t)
		e.addUser(t, alice, alicePw)
		first := e.login(t, alice, alicePw, deviceW1)
		second, err := e.users.Refresh(ctx, first.RefreshToken, deviceW1)
		if err != nil {
			t.Fatal(err)
		}
		if second.RefreshToken == first.RefreshToken || second.SessionID != first.SessionID {
			t.Fatal("AUTH-005: refresh did not rotate the token within the session")
		}
		e.clock.Advance(authn.RefreshGraceWindow + time.Second)
		_, err = e.users.Refresh(ctx, first.RefreshToken, deviceW1)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeRefreshTokenReused)
		_, err = e.users.Refresh(ctx, second.RefreshToken, deviceW1)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionInactive)
		_, err = e.users.Authenticate(ctx, second.AccessToken, deviceW1)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionInactive)
		if !e.revoked.has(first.SessionID) {
			t.Error("revocation not announced")
		}
	})
	t.Run("AUTH-005/unknown token leaves the session open", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		e := newEnv(t)
		e.addUser(t, alice, alicePw)
		pair := e.login(t, alice, alicePw, deviceW1)
		_, err := e.users.Refresh(ctx, flip(pair.RefreshToken), deviceW1)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeRefreshTokenInvalid)
		if _, err := e.users.Refresh(ctx, pair.RefreshToken, deviceW1); err != nil {
			t.Fatalf("the real token after a wrong one: %v", err)
		}
	})
	t.Run("AUTH-005/closed session", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		e := newEnv(t)
		e.addUser(t, alice, alicePw)
		pair := e.login(t, alice, alicePw, deviceW1)
		p, err := e.users.Authenticate(ctx, pair.AccessToken, deviceW1)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.users.Logout(ctx, p); err != nil {
			t.Fatal(err)
		}
		_, err = e.users.Refresh(ctx, pair.RefreshToken, deviceW1)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionInactive)
	})
	t.Run("AUTH-005/idle limit", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		e := newEnv(t)
		e.addUser(t, alice, alicePw)
		pair := e.login(t, alice, alicePw, deviceW1)
		if !pair.RefreshTokenExpiresAt.Equal(epoch.Add(authn.UserRefreshIdleTTL)) {
			t.Errorf("refresh expiry = %s, want 30 days after sign-in", pair.RefreshTokenExpiresAt)
		}
		e.clock.Advance(authn.UserRefreshIdleTTL)
		_, err := e.users.Refresh(ctx, pair.RefreshToken, deviceW1)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeRefreshTokenExpired)
		active, err := e.users.SessionActive(ctx, mustUser(t, e, alice), pair.SessionID, deviceW1)
		if err != nil || active {
			t.Errorf("session active after expiry: %v, %v", active, err)
		}
	})
	t.Run("AUTH-005/sliding window and absolute limit", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		e := newEnv(t)
		e.addUser(t, alice, alicePw)
		pair := e.login(t, alice, alicePw, deviceW1)
		for day := 20; day <= 80; day += 20 {
			e.clock.Advance(20 * 24 * time.Hour)
			next, err := e.users.Refresh(ctx, pair.RefreshToken, deviceW1)
			if err != nil {
				t.Fatalf("day %d: %v", day, err)
			}
			wantExp := e.clock.Now().Add(authn.UserRefreshIdleTTL)
			if end := epoch.Add(authn.UserSessionMaxTTL); wantExp.After(end) {
				wantExp = end
			}
			if !next.RefreshTokenExpiresAt.Equal(wantExp) {
				t.Errorf("day %d: refresh expiry %s, want %s", day, next.RefreshTokenExpiresAt, wantExp)
			}
			pair = next
		}
		e.clock.Advance(10 * 24 * time.Hour) // day 90: the session's end
		_, err := e.users.Refresh(ctx, pair.RefreshToken, deviceW1)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeRefreshTokenExpired)
	})
	t.Run("AUTH-005/retry inside the grace window gets the same successor", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		e := newEnv(t)
		e.addUser(t, alice, alicePw)
		first := e.login(t, alice, alicePw, deviceW1)
		second, err := e.users.Refresh(ctx, first.RefreshToken, deviceW1)
		if err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(authn.RefreshGraceWindow)
		retry, err := e.users.Refresh(ctx, first.RefreshToken, deviceW1)
		if err != nil || retry.RefreshToken != second.RefreshToken || !retry.RefreshTokenExpiresAt.Equal(second.RefreshTokenExpiresAt) {
			t.Fatalf("AUTH-005 retry = %+v, %v; want the successor already issued", retry, err)
		}
		if _, err := e.users.Authenticate(ctx, retry.AccessToken, deviceW1); err != nil {
			t.Fatalf("AUTH-005: access token of the retry: %v", err)
		}
		if e.revoked.has(first.SessionID) {
			t.Error("AUTH-005: a retry revoked the session")
		}
	})
	t.Run("AUTH-005/retry that is not a retry is reuse", func(t *testing.T) {
		t.Parallel()
		for name, tc := range map[string]struct {
			wait         time.Duration
			device       string
			useSuccessor bool
		}{
			"after the window":          {wait: authn.RefreshGraceWindow + time.Second, device: deviceW1},
			"from another device":       {device: deviceW2},
			"after the successor's use": {device: deviceW1, useSuccessor: true},
		} {
			e := newEnv(t)
			e.addUser(t, alice, alicePw)
			ctx := t.Context()
			first := e.login(t, alice, alicePw, deviceW1)
			second, err := e.users.Refresh(ctx, first.RefreshToken, deviceW1)
			if err != nil {
				t.Fatal(err)
			}
			if tc.useSuccessor {
				if _, err := e.users.Refresh(ctx, second.RefreshToken, deviceW1); err != nil {
					t.Fatal(err)
				}
			}
			e.clock.Advance(tc.wait)
			_, err = e.users.Refresh(ctx, first.RefreshToken, tc.device)
			if apperr.From(err).Code != authn.CodeRefreshTokenReused {
				t.Errorf("AUTH-005 %s: %v, want %s", name, err, authn.CodeRefreshTokenReused)
			}
			if active, _ := e.users.SessionActive(ctx, mustUser(t, e, alice), first.SessionID, deviceW1); active {
				t.Errorf("AUTH-005 %s: the session survived reuse", name)
			}
		}
	})
	t.Run("AUTH-005/concurrent refreshes of one token", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		e := newEnv(t)
		e.addUser(t, alice, alicePw)
		pair := e.login(t, alice, alicePw, deviceW1)

		// Hold the token's row so both refreshes queue on it, then let go:
		// the first rotates, the second is a retry inside the grace window
		// and gets the same successor. Deterministic: the test waits until
		// Postgres shows both waiting on the lock.
		hold, err := e.db.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256([]byte(pair.RefreshToken))
		if _, err := hold.Exec(ctx, "SELECT 1 FROM user_refresh_tokens WHERE token_hash = $1 FOR UPDATE", hash[:]); err != nil {
			t.Fatal(err)
		}
		results := make([]authn.TokenPair, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range results {
			wg.Go(func() {
				results[i], errs[i] = e.users.Refresh(ctx, pair.RefreshToken, deviceW1)
			})
			dbtest.Eventually(t, 10*time.Second, "refresh waiting on the token's lock", func() (bool, error) {
				var waiting int
				err := e.db.Pool().QueryRow(ctx, `
					SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
				return waiting == i+1, err
			})
		}
		if err := hold.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("AUTH-005: refresh %d: %v", i, err)
			}
		}
		if results[0].RefreshToken != results[1].RefreshToken {
			t.Errorf("AUTH-005: concurrent refreshes got different successors")
		}
		if _, err := e.users.Refresh(ctx, results[0].RefreshToken, deviceW1); err != nil {
			t.Errorf("AUTH-005: the shared successor does not work: %v", err)
		}
	})
}

// AUTH-016: a user manages their own sessions; another user's session
// looks exactly like a missing one and stays open.
func TestOwnSessions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addUser(t, alice, alicePw)
	e.addUser(t, bob, bobPw)
	ctx := t.Context()

	a1 := e.login(t, alice, alicePw, deviceW1)
	a2 := e.login(t, alice, alicePw, deviceW2)
	b3 := e.login(t, bob, bobPw, devicePhone)
	p1, err := e.users.Authenticate(ctx, a1.AccessToken, deviceW1)
	if err != nil {
		t.Fatal(err)
	}

	list, err := e.users.ListSessions(ctx, p1)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListSessions() = %v, %v; want two", list, err)
	}
	for _, s := range list {
		if s.Current != (s.ID == a1.SessionID) || s.Platform != "WEB" {
			t.Errorf("session %+v", s)
		}
	}

	// AUTH-016: someone else's session is a missing one.
	{
		_, errOther := e.users.GetSession(ctx, p1, b3.SessionID)
		_, errMissing := e.users.GetSession(ctx, p1, uuid.Must(uuid.NewV7()))
		wantErr(t, errOther, apperr.KindNotFound, authn.CodeSessionNotFound)
		if errOther.Error() != errMissing.Error() {
			t.Errorf("errors differ: %q vs %q", errOther, errMissing)
		}
		err := e.users.RevokeSession(ctx, p1, b3.SessionID)
		wantErr(t, err, apperr.KindNotFound, authn.CodeSessionNotFound)
		if _, err := e.users.Authenticate(ctx, b3.AccessToken, devicePhone); err != nil {
			t.Errorf("AUTH-016: bob's session after alice tried to close it: %v", err)
		}
	}
	// AUTH-016: revoking the others spares the caller.
	{
		if err := e.users.RevokeOtherSessions(ctx, p1); err != nil {
			t.Fatal(err)
		}
		_, err := e.users.Authenticate(ctx, a2.AccessToken, deviceW2)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionInactive)
		if _, err := e.users.Authenticate(ctx, a1.AccessToken, deviceW1); err != nil {
			t.Errorf("caller's session: %v", err)
		}
		if !e.revoked.has(a2.SessionID) || e.revoked.has(a1.SessionID) {
			t.Error("wrong sessions announced as revoked")
		}
		_, err = e.users.GetSession(ctx, p1, a2.SessionID)
		wantErr(t, err, apperr.KindNotFound, authn.CodeSessionNotFound)
	}
	// AUTH-016: the caller closes its own session.
	if err := e.users.RevokeSession(ctx, p1, a1.SessionID); err != nil {
		t.Fatal(err)
	}
	_, err = e.users.Authenticate(ctx, a1.AccessToken, deviceW1)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionInactive)
}

// A suspended account's sessions stop at once and it cannot sign in; the
// error says why only to someone who knows the password.
func TestSuspendedUser(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.addUser(t, alice, alicePw)
	ctx := t.Context()
	pair := e.login(t, alice, alicePw, deviceW1)

	if _, err := e.db.Pool().Exec(ctx, "UPDATE active_users SET status = 'SUSPENDED' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	_, err := e.users.Authenticate(ctx, pair.AccessToken, deviceW1)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionInactive)
	_, err = e.users.Login(ctx, authn.LoginInput{Email: alice, Password: alicePw, DeviceKey: deviceW1})
	wantErr(t, err, apperr.KindForbidden, authn.CodeAccountSuspended)
	_, err = e.users.Login(ctx, authn.LoginInput{Email: alice, Password: wrongPw, DeviceKey: deviceW1})
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeEmailOrPasswordMismatch)
}

// The realtime session check: open on its own device for its own user,
// closed for anything else (ADR-0022 §2).
func TestSessionActive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	userID := e.addUser(t, alice, alicePw)
	ctx := t.Context()
	pair := e.login(t, alice, alicePw, deviceW1)

	for name, tc := range map[string]struct {
		user    uuid.UUID
		session uuid.UUID
		device  string
		want    bool
	}{
		"own":          {userID, pair.SessionID, deviceW1, true},
		"other device": {userID, pair.SessionID, deviceW2, false},
		"other user":   {uuid.Must(uuid.NewV7()), pair.SessionID, deviceW1, false},
		"missing":      {userID, uuid.Must(uuid.NewV7()), deviceW1, false},
	} {
		got, err := e.users.SessionActive(ctx, tc.user, tc.session, tc.device)
		if err != nil || got != tc.want {
			t.Errorf("%s: SessionActive() = %v, %v; want %v", name, got, err, tc.want)
		}
	}
}

// Purge removes sessions that ended past the retention, with their tokens,
// and old attempts; open sessions stay.
func TestPurge(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addUser(t, alice, alicePw)
	ctx := t.Context()
	open := e.login(t, alice, alicePw, deviceW2)
	closed := e.login(t, alice, alicePw, deviceW1)
	p, err := e.users.Authenticate(ctx, closed.AccessToken, deviceW1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.users.Logout(ctx, p); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(authn.EndedSessionRetention + time.Second)
	sessions, attempts, err := e.users.Purge(ctx)
	if err != nil || sessions != 1 || attempts != 0 {
		t.Fatalf("Purge() = %d, %d, %v; want 1 session, 0 attempts", sessions, attempts, err)
	}
	if active, err := e.users.SessionActive(ctx, mustUser(t, e, alice), open.SessionID, deviceW2); err != nil || !active {
		t.Errorf("open session after purge: %v, %v", active, err)
	}
	e.clock.Advance(authn.LoginAttemptRetention)
	if _, attempts, err := e.users.Purge(ctx); err != nil || attempts != 2 {
		t.Errorf("attempt purge = %d, %v; want the 2 sign-ins", attempts, err)
	}
}

func mustUser(t *testing.T, e *env, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.db.Pool().QueryRow(t.Context(), "SELECT id FROM active_users WHERE email = $1", email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// flip changes one character in the middle of a token.
func flip(s string) string {
	b := []byte(s)
	i := len(b) / 2
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

type alwaysActive struct{}

func (alwaysActive) Active(context.Context, realtime.Principal) (bool, error) { return true, nil }

// Marketplace lock (ADR-0025 §5, NIST SP 800-63B): the hundredth
// consecutive failure on a target locks it for an hour. While locked the
// right password gets the wrong-password answer and pays the same
// verification; refused attempts do not extend the lock. The HTTP body is
// compared byte for byte in internal/app.
func TestLoginLocksAfterAHundredFailures(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addUser(t, alice, alicePw)
	ctx := t.Context()
	target := e.users.Target(alice)
	for i := range authn.UserLockThreshold - 1 {
		_, err := e.db.Pool().Exec(ctx,
			"INSERT INTO login_attempts (id, principal_kind, target_hash, outcome, created_at) VALUES ($1, 'USER', $2, 'FAILED', $3)",
			uuid.Must(uuid.NewV7()), target, epoch.Add(-time.Duration(authn.UserLockThreshold-i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	_, wrong := e.users.Login(ctx, authn.LoginInput{Email: alice, Password: wrongPw, DeviceKey: deviceW1})
	wantErr(t, wrong, apperr.KindUnauthenticated, authn.CodeEmailOrPasswordMismatch)
	if got := e.lockouts(t, "user", "target"); got != 1 {
		t.Errorf("lockouts metric = %v, want 1", got)
	}

	before := e.passwords.verifies.Load()
	_, locked := e.users.Login(ctx, authn.LoginInput{Email: alice, Password: alicePw, DeviceKey: deviceW1})
	if locked == nil || locked.Error() != wrong.Error() {
		t.Fatalf("locked: %v, want the wrong-password error %v", locked, wrong)
	}
	if got := e.passwords.verifies.Load() - before; got != 1 {
		t.Errorf("a locked sign-in paid %d verifications, want 1", got)
	}
	e.clock.Advance(authn.UserLockDuration - time.Second)
	if _, err := e.users.Login(ctx, authn.LoginInput{Email: alice, Password: alicePw, DeviceKey: deviceW1}); err == nil {
		t.Fatal("signed in while locked")
	}
	if row := e.attempts(t, "USER", alice); row.Locked != 2 || row.Failed != authn.UserLockThreshold {
		t.Errorf("attempts = %+v, want 2 LOCKED that do not count as failures", row)
	}

	e.clock.Advance(time.Second)
	e.login(t, alice, alicePw, deviceW1)
	if _, err := e.users.Login(ctx, authn.LoginInput{Email: alice, Password: wrongPw, DeviceKey: deviceW1}); apperr.From(err).Code != authn.CodeEmailOrPasswordMismatch {
		t.Fatalf("after the lock: %v", err)
	}
	// The success reset the counter: one failure is not a lock.
	e.login(t, alice, alicePw, deviceW1)
}
