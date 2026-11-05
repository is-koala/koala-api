//go:build integration

package authn_test

import (
	"fmt"
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
	officeIP   = "192.0.2.10"
	homeIP     = "198.51.100.7"
)

// staffLogin signs the ops member in from client.
func (e *env) staffLogin(t *testing.T, password, client string) authn.StaffTokenPair {
	t.Helper()
	pair, err := e.staff.Login(t.Context(), opsEmail, password, client)
	if err != nil {
		t.Fatalf("staff Login(%s from %s) = %v", opsEmail, client, err)
	}
	return pair
}

// lockState reads the member's account counter and backoff.
func (e *env) lockState(t *testing.T, id uuid.UUID) (int32, *time.Time) {
	t.Helper()
	var n int32
	var until *time.Time
	err := e.db.Pool().QueryRow(t.Context(),
		"SELECT failed_login_attempts, backoff_until FROM active_staff_members WHERE id = $1", id).Scan(&n, &until)
	if err != nil {
		t.Fatal(err)
	}
	return n, until
}

// addressLock reads the member's lock for the office address, nil when
// there is none.
func (e *env) addressLock(t *testing.T, id uuid.UUID) *time.Time {
	t.Helper()
	var until *time.Time
	err := e.db.Pool().QueryRow(t.Context(),
		"SELECT max(locked_until) FROM staff_login_sources WHERE staff_id = $1 AND client_key = $2", id, officeIP).Scan(&until)
	if err != nil {
		t.Fatal(err)
	}
	return until
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
		_, err := e.staff.Login(ctx, tc.email, tc.pw, officeIP)
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
		if row := e.attempts(t, "STAFF", addr); row.Succeeded != 0 || row.Failed != 1 {
			t.Errorf("AUTH-010 %s attempts: %+v", addr, row)
		}
	}
}

// AUTH-011 (ADR-0025 §6): five wrong passwords from one address lock the
// account for that address for 30 minutes. While locked the right
// password from that address fails too, nothing moves the lock, and the
// answer is the plain failure. Another address is not locked out. A
// success clears the counters. The HTTP bodies are compared byte for byte
// in internal/app (TestStaffLockBodyIsTheFailureBody).
func TestStaffLockout(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.addStaff(t, opsEmail, authn.StaffActive, false)
	ctx := t.Context()

	for i := 1; i <= authn.StaffLockThreshold; i++ {
		_, err := e.staff.Login(ctx, opsEmail, wrongPw, officeIP)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffLoginFailed)
		if i < authn.StaffLockThreshold && e.addressLock(t, id) != nil {
			t.Fatalf("AUTH-011: locked after %d failures", i)
		}
	}
	locked := e.addressLock(t, id)
	if locked == nil || !locked.Equal(epoch.Add(authn.StaffLockDuration)) {
		t.Fatalf("AUTH-011: lock = %v, want %s", locked, epoch.Add(authn.StaffLockDuration))
	}
	if got := e.lockouts(t, "staff", "address"); got != 1 {
		t.Errorf("AUTH-011: address lockouts metric = %v, want 1", got)
	}

	e.clock.Advance(5 * time.Minute)
	for _, pw := range []string{opsPw, wrongPw} {
		_, err := e.staff.Login(ctx, opsEmail, pw, officeIP)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffLoginFailed)
		if until := e.addressLock(t, id); until == nil || !until.Equal(*locked) {
			t.Errorf("AUTH-011: an attempt while locked moved the lock to %v", until)
		}
	}
	if row := e.attempts(t, "STAFF", opsEmail); row.Locked != 2 {
		t.Errorf("AUTH-011: attempts while locked = %+v, want 2 LOCKED", row)
	}

	// The lock is the address's: the member signs in from home.
	e.staffLogin(t, opsPw, homeIP)
	if n, until := e.lockState(t, id); n != 0 || until != nil {
		t.Errorf("AUTH-011: account counter after a success = %d, %v", n, until)
	}

	e.clock.Advance(authn.StaffLockDuration)
	e.staffLogin(t, opsPw, officeIP)
	if until := e.addressLock(t, id); until != nil {
		t.Errorf("AUTH-011: the office lock survived a success: %v", until)
	}
}

// Account-wide backoff (ADR-0025 §6): from the tenth consecutive wrong
// password on the account, from any address, each failure sets a
// doubling backoff during which every attempt is refused; there is no
// hard lock.
func TestStaffBackoff(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.addStaff(t, opsEmail, authn.StaffActive, false)
	ctx := t.Context()

	// Four addresses, fewer than five failures each: no address lock.
	for i := range authn.StaffBackoffAfter {
		client := []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"}[i%4]
		if _, err := e.staff.Login(ctx, opsEmail, wrongPw, client); err == nil {
			t.Fatal("wrong password accepted")
		}
	}
	n, until := e.lockState(t, id)
	if n != authn.StaffBackoffAfter || until == nil || !until.Equal(epoch.Add(authn.StaffBackoffBase)) {
		t.Fatalf("after %d failures: counter %d, backoff %v; want %s", authn.StaffBackoffAfter, n, until, authn.StaffBackoffBase)
	}
	// During the backoff even the right password from a clean address fails.
	_, err := e.staff.Login(ctx, opsEmail, opsPw, homeIP)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffLoginFailed)

	e.clock.Advance(authn.StaffBackoffBase)
	if _, err := e.staff.Login(ctx, opsEmail, wrongPw, homeIP); err == nil {
		t.Fatal("wrong password accepted")
	}
	if _, until := e.lockState(t, id); until == nil || !until.Equal(e.clock.Now().Add(2*authn.StaffBackoffBase)) {
		t.Errorf("second backoff = %v, want twice the first", until)
	}
	if got := e.lockouts(t, "staff", "backoff"); got != 2 {
		t.Errorf("backoff metric = %v, want 2", got)
	}

	e.clock.Advance(authn.StaffBackoffMax)
	e.staffLogin(t, opsPw, homeIP)
	if n, until := e.lockState(t, id); n != 0 || until != nil {
		t.Errorf("after a success: counter %d, backoff %v", n, until)
	}
}

// AUTH-011 (ADR-0025 §6): the account backoff holds back unknown sources
// only. However many addresses an attacker fails from, the member still
// signs in from an address they signed in from within KnownSourceWindow;
// once that success is older than the window the address is unknown
// again.
func TestStaffKnownSourceIsNotBackedOff(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.addStaff(t, opsEmail, authn.StaffActive, false)
	ctx := t.Context()
	e.staffLogin(t, opsPw, officeIP)

	attack := func(round int) {
		t.Helper()
		ctx := t.Context()
		// One failure per address: no address lock, only the backoff.
		for i := range authn.StaffBackoffAfter {
			_, err := e.staff.Login(ctx, opsEmail, wrongPw, fmt.Sprintf("203.0.%d.%d", 113+round, i+1))
			wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffLoginFailed)
		}
		if _, until := e.lockState(t, id); !e.clock.Now().Before(derefTime(until)) {
			t.Fatalf("AUTH-011: no backoff after %d failures from other addresses: %v", authn.StaffBackoffAfter, until)
		}
	}
	attack(0)
	_, err := e.staff.Login(ctx, opsEmail, opsPw, homeIP)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffLoginFailed)
	if _, err := e.staff.Login(ctx, opsEmail, opsPw, officeIP); err != nil {
		t.Fatalf("AUTH-011: the attacker kept the member out of a known-good address: %v", err)
	}

	e.clock.Advance(authn.KnownSourceWindow)
	attack(1)
	_, err = e.staff.Login(ctx, opsEmail, opsPw, officeIP)
	if apperr.From(err).Code != authn.CodeStaffLoginFailed {
		t.Fatalf("AUTH-011: an address last used %s ago is still exempt: %v", authn.KnownSourceWindow, err)
	}
}

// derefTime is *p, or the zero time.
func derefTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

// AUTH-010, AUTH-011 (ADR-0025 §6): a sign-in refused unexamined (a
// disabled account, a locked address, the account backoff, an unknown
// address) is verified against the dummy hash only, whether the password
// is right or not, so its time does not tell; the right password of a
// hash under old parameters pays no rehash there. A wrong password on an
// open account pays one verification of the stored hash, the same work,
// and only a success pays the rehash.
func TestStaffRefusedSignInsCostTheSame(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	old, err := authn.NewPasswords(authn.Params{MemoryKiB: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 1)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := old.Hash(ctx, opsPw)
	if err != nil {
		t.Fatal(err)
	}
	const (
		disabled = "gone@iskoala.com"
		locked   = "locked@iskoala.com"
		slow     = "slow@iskoala.com"
		open     = "open@iskoala.com"
	)
	e.addStaffHashed(t, disabled, authn.StaffDisabled, false, hash)
	e.addStaffHashed(t, locked, authn.StaffActive, false, hash)
	e.addStaffHashed(t, slow, authn.StaffActive, false, hash)
	e.addStaffHashed(t, open, authn.StaffActive, false, hash)
	for range authn.StaffLockThreshold {
		_, _ = e.staff.Login(ctx, locked, wrongPw, officeIP)
	}
	for i := range authn.StaffBackoffAfter {
		_, _ = e.staff.Login(ctx, slow, wrongPw, fmt.Sprintf("198.51.100.%d", i+1))
	}

	refused := cost{dummies: 1}
	for _, tc := range []struct {
		name, email, pw, client string
		want                    cost
	}{
		{"AUTH-010 disabled, right password", disabled, opsPw, officeIP, refused},
		{"AUTH-010 disabled, wrong password", disabled, wrongPw, officeIP, refused},
		{"AUTH-010 unknown address", "p3@example.com", opsPw, officeIP, refused},
		{"AUTH-011 locked address, right password", locked, opsPw, officeIP, refused},
		{"AUTH-011 locked address, wrong password", locked, wrongPw, officeIP, refused},
		{"AUTH-011 backoff, right password", slow, opsPw, homeIP, refused},
		{"AUTH-011 backoff, wrong password", slow, wrongPw, homeIP, refused},
		{"AUTH-010 open account, wrong password", open, wrongPw, officeIP, cost{verifies: 1}},
	} {
		var err error
		got := e.passwords.costOf(func() { _, err = e.staff.Login(ctx, tc.email, tc.pw, tc.client) })
		if apperr.From(err).Code != authn.CodeStaffLoginFailed {
			t.Errorf("%s: %v, want %s", tc.name, err, authn.CodeStaffLoginFailed)
		}
		if got != tc.want {
			t.Errorf("%s: cost %+v, want %+v", tc.name, got, tc.want)
		}
	}
	var err2 error
	got := e.passwords.costOf(func() { _, err2 = e.staff.Login(ctx, open, opsPw, officeIP) })
	if err2 != nil || got != (cost{verifies: 1, hashes: 1}) {
		t.Errorf("AUTH-011 success on an old hash: %v, cost %+v; want one verification and the rehash", err2, got)
	}
}

// ADR-0025 §6: a password change is guarded like a sign-in from the
// request's address. From a locked address, or under the account backoff
// from an unknown address, it is refused unexamined even with the right
// current password; a wrong current password that backs the account off
// ends the session.
func TestStaffPasswordChangeIsGuarded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addStaff(t, opsEmail, authn.StaffActive, false)
	ctx := t.Context()
	pair := e.staffLogin(t, opsPw, officeIP)
	p, err := e.staff.Authenticate(ctx, pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	refused := cost{dummies: 1}

	for range authn.StaffLockThreshold {
		_, _ = e.staff.Login(ctx, opsEmail, wrongPw, homeIP)
	}
	got := e.passwords.costOf(func() { err = e.staff.ChangePassword(ctx, p, opsPw, newStaffPw, homeIP) })
	wantErr(t, err, apperr.KindForbidden, authn.CodeStaffCurrentPasswordMismatch)
	if got != refused {
		t.Errorf("change from a locked address: cost %+v, want %+v", got, refused)
	}

	for i := range authn.StaffBackoffAfter {
		_, _ = e.staff.Login(ctx, opsEmail, wrongPw, fmt.Sprintf("198.51.100.%d", i+1))
	}
	got = e.passwords.costOf(func() { err = e.staff.ChangePassword(ctx, p, opsPw, newStaffPw, "192.0.2.77") })
	wantErr(t, err, apperr.KindForbidden, authn.CodeStaffCurrentPasswordMismatch)
	if got != refused {
		t.Errorf("change under the backoff from an unknown address: cost %+v, want %+v", got, refused)
	}
	if _, err := e.staff.Authenticate(ctx, pair.AccessToken); err != nil {
		t.Fatalf("a refused change ended the session: %v", err)
	}

	// The office is a known-good source: examined, and a wrong current
	// password there backs the account off further, which ends the session.
	err = e.staff.ChangePassword(ctx, p, wrongPw, newStaffPw, officeIP)
	wantErr(t, err, apperr.KindForbidden, authn.CodeStaffCurrentPasswordMismatch)
	_, err = e.staff.Authenticate(ctx, pair.AccessToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffSessionEnded)
	e.staffLogin(t, opsPw, officeIP)
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
	pair := e.staffLogin(t, opsPw, officeIP)
	userPair := e.login(t, alice, alicePw, deviceW1)

	if _, err := e.staff.Authenticate(ctx, pair.AccessToken); err != nil {
		t.Fatalf("AUTH-012: fresh staff token: %v", err)
	}
	_, err := e.staff.Authenticate(ctx, pair.RefreshToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeTokenInvalid)
	_, err = e.staff.Authenticate(ctx, userPair.AccessToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeTokenInvalid)
	_, err = e.users.Authenticate(ctx, pair.AccessToken, deviceW1)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeTokenInvalid)

	e.clock.Advance(authn.AccessTokenTTL + authn.ClockSkew)
	_, err = e.staff.Authenticate(ctx, pair.AccessToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeTokenExpired)
	next, err := e.staff.Refresh(ctx, pair.RefreshToken)
	if err != nil {
		t.Fatalf("AUTH-012: refresh within the idle limit: %v", err)
	}
	e.clock.Advance(authn.StaffRefreshIdleTTL)
	_, err = e.staff.Refresh(ctx, next.RefreshToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeRefreshTokenExpired)
}

// AUTH-013: logout and a password change end every token of the account,
// on every device, access and refresh, the one that asked included. The
// change checks the current password, and the new one follows AUTH-009.
func TestStaffLogoutAndPasswordChange(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addStaff(t, opsEmail, authn.StaffActive, false)
	ctx := t.Context()

	laptop := e.staffLogin(t, opsPw, officeIP)
	desktop := e.staffLogin(t, opsPw, homeIP)
	p, err := e.staff.Authenticate(ctx, laptop.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.staff.Logout(ctx, p); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string]authn.StaffTokenPair{"laptop": laptop, "desktop": desktop} {
		if _, err := e.staff.Authenticate(ctx, pair.AccessToken); apperr.From(err).Code != authn.CodeStaffSessionEnded {
			t.Errorf("AUTH-013: %s access token after logout: %v", name, err)
		}
		if _, err := e.staff.Refresh(ctx, pair.RefreshToken); apperr.From(err).Code != authn.CodeStaffSessionEnded {
			t.Errorf("AUTH-013: %s refresh token after logout: %v", name, err)
		}
	}

	again := e.staffLogin(t, opsPw, officeIP)
	other := e.staffLogin(t, opsPw, homeIP)
	p, err = e.staff.Authenticate(ctx, again.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	err = e.staff.ChangePassword(ctx, p, wrongPw, newStaffPw, officeIP)
	wantErr(t, err, apperr.KindForbidden, authn.CodeStaffCurrentPasswordMismatch)
	err = e.staff.ChangePassword(ctx, p, opsPw, "on dört harfli", officeIP)
	if apperr.From(err).Code != authn.CodePasswordPolicy {
		t.Errorf("AUTH-009 on the staff surface: %v, want %s", err, authn.CodePasswordPolicy)
	}
	if _, err := e.staff.Authenticate(ctx, again.AccessToken); err != nil {
		t.Fatalf("AUTH-013: refused changes must not end the session: %v", err)
	}

	if err := e.staff.ChangePassword(ctx, p, opsPw, newStaffPw, officeIP); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string]authn.StaffTokenPair{"changer": again, "other device": other} {
		if _, err := e.staff.Authenticate(ctx, pair.AccessToken); apperr.From(err).Code != authn.CodeStaffSessionEnded {
			t.Errorf("AUTH-013: %s access token after the change: %v", name, err)
		}
		if _, err := e.staff.Refresh(ctx, pair.RefreshToken); apperr.From(err).Code != authn.CodeStaffSessionEnded {
			t.Errorf("AUTH-013: %s refresh token after the change: %v", name, err)
		}
	}
	_, err = e.staff.Login(ctx, opsEmail, opsPw, officeIP)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffLoginFailed)
	e.staffLogin(t, newStaffPw, officeIP)
}

// A wrong current password counts like a wrong sign-in password; the
// fifth from one address locks it and ends the session (ADR-0025 §6).
func TestStaffPasswordGuessingEndsTheSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.addStaff(t, opsEmail, authn.StaffActive, false)
	ctx := t.Context()
	pair := e.staffLogin(t, opsPw, officeIP)
	p, err := e.staff.Authenticate(ctx, pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= authn.StaffLockThreshold; i++ {
		err := e.staff.ChangePassword(ctx, p, wrongPw, newStaffPw, officeIP)
		wantErr(t, err, apperr.KindForbidden, authn.CodeStaffCurrentPasswordMismatch)
		_, authErr := e.staff.Authenticate(ctx, pair.AccessToken)
		if ended := authErr != nil; ended != (i == authn.StaffLockThreshold) {
			t.Fatalf("after %d wrong current passwords: session ended = %v", i, ended)
		}
	}
	if e.addressLock(t, id) == nil {
		t.Error("the address is not locked")
	}
	_, err = e.staff.Login(ctx, opsEmail, opsPw, officeIP)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffLoginFailed)
}

// AUTH-014 (service part): a new account signs in with the must-change
// flag; after the change the flag is gone. The gate itself is the
// middleware's (internal/app TestStaffMustChangePassword).
func TestStaffMustChangeFlag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addStaff(t, opsEmail, authn.StaffActive, true)
	ctx := t.Context()

	first := e.staffLogin(t, opsPw, officeIP)
	if !first.MustChangePassword {
		t.Fatal("AUTH-014: new account signed in without the must-change flag")
	}
	p, err := e.staff.Authenticate(ctx, first.AccessToken)
	if err != nil || !p.MustChangePassword {
		t.Fatalf("AUTH-014: principal = %+v, %v", p, err)
	}
	if err := e.staff.ChangePassword(ctx, p, opsPw, newStaffPw, officeIP); err != nil {
		t.Fatal(err)
	}
	if second := e.staffLogin(t, newStaffPw, officeIP); second.MustChangePassword {
		t.Error("AUTH-014: flag still set after the change")
	}
}

// Staff refresh rotation, the grace window and reuse detection mirror the
// marketplace's.
func TestStaffRefreshReuse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addStaff(t, opsEmail, authn.StaffActive, false)
	ctx := t.Context()

	first := e.staffLogin(t, opsPw, officeIP)
	second, err := e.staff.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := e.staff.Refresh(ctx, first.RefreshToken)
	if err != nil || retry.RefreshToken != second.RefreshToken {
		t.Fatalf("retry inside the grace window = %v; want the same successor", err)
	}
	e.clock.Advance(authn.RefreshGraceWindow + time.Second)
	_, err = e.staff.Refresh(ctx, first.RefreshToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeRefreshTokenReused)
	_, err = e.staff.Refresh(ctx, second.RefreshToken)
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeStaffSessionEnded)
	if n, locks, err := e.staff.Purge(ctx); err != nil || n != 0 || locks != 0 {
		t.Errorf("Purge() before retention = %d, %d, %v", n, locks, err)
	}
}
