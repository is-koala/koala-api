//go:build integration

package authn_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/apperr"
)

const (
	opsEmail   = "ops@iskoala.com"
	opsPw      = "ekip üyesinin uzun parolası"
	newStaffPw = "yepyeni uzun bir parola"
)

func (e *env) staffLogin(t *testing.T, email, password string) authn.StaffTokenPair {
	t.Helper()
	pair, err := e.staff.Login(t.Context(), email, password)
	if err != nil {
		t.Fatalf("staff Login(%s) = %v", email, err)
	}
	return pair
}

// lockState reads the staff member's counter and lock.
func (e *env) lockState(t *testing.T, id uuid.UUID) (int32, *time.Time) {
	t.Helper()
	var n int32
	var until *time.Time
	err := e.db.Pool().QueryRow(t.Context(),
		"SELECT failed_login_attempts, locked_until FROM active_staff_members WHERE id = $1", id).Scan(&n, &until)
	if err != nil {
		t.Fatal(err)
	}
	return n, until
}

// AUTH-010: a wrong password, a disabled account (even with its right
// password) and an address that is no staff member are the same error,
// and each pays one verification.
func TestStaffLoginConcealment(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addStaff(t, opsEmail, authn.StaffActive, false)
	e.addStaff(t, "gone@iskoala.com", authn.StaffDisabled, false)
	ctx := t.Context()

	var msgs []string
	for _, tc := range []struct{ email, pw string }{
		{opsEmail, wrongPw},
		{"gone@iskoala.com", opsPw},
		{"p3@example.com", opsPw},
	} {
		before := e.passwords.verifies.Load()
		_, err := e.staff.Login(ctx, tc.email, tc.pw)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffLoginFailed)
		msgs = append(msgs, err.Error())
		if got := e.passwords.verifies.Load() - before; got != 1 {
			t.Errorf("AUTH-010 %s: %d verifications, want 1", tc.email, got)
		}
	}
	if msgs[0] != msgs[1] || msgs[1] != msgs[2] {
		t.Errorf("AUTH-010: errors differ: %q", msgs)
	}
	for _, addr := range []string{opsEmail, "gone@iskoala.com", "p3@example.com"} {
		if ok, failed := e.attempts(t, "STAFF", addr); ok != 0 || failed != 1 {
			t.Errorf("%s attempts: %d ok, %d failed", addr, ok, failed)
		}
	}
}

// AUTH-011: five wrong passwords lock the account for 15 minutes. While
// locked, the right password fails too, no attempt moves the lock, and
// the answer is the plain failure. A success resets the counter.
func TestStaffLockout(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.addStaff(t, opsEmail, authn.StaffActive, false)
	ctx := t.Context()

	for i := 1; i <= authn.StaffLockThreshold; i++ {
		_, err := e.staff.Login(ctx, opsEmail, wrongPw)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffLoginFailed)
		n, until := e.lockState(t, id)
		if i < authn.StaffLockThreshold && (n != int32(i) || until != nil) {
			t.Fatalf("AUTH-011: after %d failures: counter %d, lock %v", i, n, until)
		}
	}
	_, lockedUntil := e.lockState(t, id)
	if lockedUntil == nil || !lockedUntil.Equal(epoch.Add(authn.StaffLockDuration)) {
		t.Fatalf("AUTH-011: lock = %v, want %s", lockedUntil, epoch.Add(authn.StaffLockDuration))
	}

	e.clock.Advance(5 * time.Minute)
	for _, pw := range []string{opsPw, wrongPw} {
		_, err := e.staff.Login(ctx, opsEmail, pw)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffLoginFailed)
		if _, until := e.lockState(t, id); until == nil || !until.Equal(*lockedUntil) {
			t.Errorf("AUTH-011: an attempt while locked moved the lock to %v", until)
		}
	}

	e.clock.Advance(authn.StaffLockDuration)
	e.staffLogin(t, opsEmail, opsPw)
	if n, until := e.lockState(t, id); n != 0 || until != nil {
		t.Errorf("AUTH-011: after a success: counter %d, lock %v", n, until)
	}

	// Another member: two failures, then a success clears the counter.
	other := e.addStaff(t, "finance@iskoala.com", authn.StaffActive, false)
	for range 2 {
		if _, err := e.staff.Login(ctx, "finance@iskoala.com", wrongPw); err == nil {
			t.Fatal("wrong password accepted")
		}
	}
	e.staffLogin(t, "finance@iskoala.com", opsPw)
	if n, _ := e.lockState(t, other); n != 0 {
		t.Errorf("AUTH-011: counter after success = %d", n)
	}
}

// AUTH-012 (token part): an expired staff token and a refresh token used
// as an access token are refused; a marketplace token is not a staff one.
// Permissions are read from the database per request by the staff RBAC
// (migrations 0040+), which this token never carries.
func TestStaffTokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addStaff(t, opsEmail, authn.StaffActive, false)
	e.addUser(t, alice, alicePw)
	ctx := t.Context()
	pair := e.staffLogin(t, opsEmail, opsPw)
	userPair := e.login(t, alice, alicePw, deviceW1)

	if _, err := e.staff.Authenticate(ctx, pair.AccessToken); err != nil {
		t.Fatalf("fresh staff token: %v", err)
	}
	_, err := e.staff.Authenticate(ctx, pair.RefreshToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeTokenInvalid)
	_, err = e.staff.Authenticate(ctx, userPair.AccessToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeTokenInvalid)
	_, err = e.users.Authenticate(ctx, pair.AccessToken, deviceW1)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeTokenInvalid)

	e.clock.Advance(authn.AccessTokenTTL + time.Second)
	_, err = e.staff.Authenticate(ctx, pair.AccessToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeTokenExpired)
	next, err := e.staff.Refresh(ctx, pair.RefreshToken)
	if err != nil {
		t.Fatalf("refresh within the idle limit: %v", err)
	}
	e.clock.Advance(authn.StaffRefreshIdleTTL)
	_, err = e.staff.Refresh(ctx, next.RefreshToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeRefreshTokenExpired)
}

// AUTH-013: logout and a password change end every token of the account,
// on every device, the one that asked included. The change checks the
// current password, and the new one follows AUTH-009.
func TestStaffLogoutAndPasswordChange(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addStaff(t, opsEmail, authn.StaffActive, false)
	ctx := t.Context()

	laptop := e.staffLogin(t, opsEmail, opsPw)
	desktop := e.staffLogin(t, opsEmail, opsPw)
	p, err := e.staff.Authenticate(ctx, laptop.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.staff.Logout(ctx, p); err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"laptop": laptop.AccessToken, "desktop": desktop.AccessToken} {
		_, err := e.staff.Authenticate(ctx, tok)
		if apperr.From(err).Code != authn.CodeStaffSessionEnded {
			t.Errorf("AUTH-013: %s access token after logout: %v", name, err)
		}
	}
	for name, tok := range map[string]string{"laptop": laptop.RefreshToken, "desktop": desktop.RefreshToken} {
		_, err := e.staff.Refresh(ctx, tok)
		if apperr.From(err).Code != authn.CodeStaffSessionEnded {
			t.Errorf("AUTH-013: %s refresh token after logout: %v", name, err)
		}
	}

	again := e.staffLogin(t, opsEmail, opsPw)
	p, err = e.staff.Authenticate(ctx, again.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	err = e.staff.ChangePassword(ctx, p, wrongPw, newStaffPw)
	wantErr(t, err, apperr.KindForbidden, authn.CodeStaffCurrentPasswordMismatch)
	err = e.staff.ChangePassword(ctx, p, opsPw, "onbir harf")
	wantErr(t, err, apperr.KindInvalid, authn.CodePasswordPolicy) // AUTH-009 on the staff surface
	if _, err := e.staff.Authenticate(ctx, again.AccessToken); err != nil {
		t.Fatalf("refused changes must not end the session: %v", err)
	}

	if err := e.staff.ChangePassword(ctx, p, opsPw, newStaffPw); err != nil {
		t.Fatal(err)
	}
	_, err = e.staff.Authenticate(ctx, again.AccessToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffSessionEnded)
	_, err = e.staff.Login(ctx, opsEmail, opsPw)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffLoginFailed)
	e.staffLogin(t, opsEmail, newStaffPw)
}

// AUTH-014 (service part): a new account signs in with the must-change
// flag; after the change the flag is gone. The gate itself is the
// middleware's (internal/app TestStaffMustChangePassword).
func TestStaffMustChangeFlag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addStaff(t, opsEmail, authn.StaffActive, true)
	ctx := t.Context()

	first := e.staffLogin(t, opsEmail, opsPw)
	if !first.MustChangePassword {
		t.Fatal("AUTH-014: new account signed in without the must-change flag")
	}
	p, err := e.staff.Authenticate(ctx, first.AccessToken)
	if err != nil || !p.MustChangePassword {
		t.Fatalf("principal = %+v, %v", p, err)
	}
	if err := e.staff.ChangePassword(ctx, p, opsPw, newStaffPw); err != nil {
		t.Fatal(err)
	}
	if second := e.staffLogin(t, opsEmail, newStaffPw); second.MustChangePassword {
		t.Error("AUTH-014: flag still set after the change")
	}
}

// Staff refresh rotation and reuse detection mirror the marketplace's.
func TestStaffRefreshReuse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addStaff(t, opsEmail, authn.StaffActive, false)
	ctx := t.Context()

	first := e.staffLogin(t, opsEmail, opsPw)
	second, err := e.staff.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.staff.Refresh(ctx, first.RefreshToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeRefreshTokenReused)
	_, err = e.staff.Refresh(ctx, second.RefreshToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffSessionEnded)
	if n, err := e.staff.Purge(ctx); err != nil || n != 0 {
		t.Errorf("Purge() before retention = %d, %v", n, err)
	}
}
