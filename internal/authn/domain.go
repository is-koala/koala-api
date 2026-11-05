// Package authn authenticates marketplace users and staff (ADR-0015,
// ADR-0025): password sign-in, device-bound user sessions, rotating opaque
// refresh tokens with reuse detection (K12), short-lived EdDSA access
// tokens of two classes, the staff lockout, must_change_password and
// token_version, and the per-request session check.
//
// Authorization is not decided here (ADR-0014): authn only says who the
// caller is and that the session is alive.
package authn

import (
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// Error codes. Each is in api/components/problem.yaml's ErrorCode.
const (
	// CodeEmailOrPasswordMismatch: unknown address, wrong password or an
	// account without a password; the three are indistinguishable
	// (AUTH-001).
	CodeEmailOrPasswordMismatch apperr.Code = "AUTH_EMAIL_OR_PASSWORD_MISMATCH"
	// CodeAccountSuspended: the password was right but the account is
	// suspended. Only someone who knows the password learns it.
	CodeAccountSuspended apperr.Code = "AUTH_ACCOUNT_SUSPENDED"
	// CodeTokenMissing: a protected operation without a bearer token.
	CodeTokenMissing apperr.Code = "AUTH_TOKEN_MISSING"
	// CodeTokenInvalid: a malformed, forged, foreign-class or not yet valid
	// access token.
	CodeTokenInvalid apperr.Code = "AUTH_TOKEN_INVALID"
	// CodeTokenExpired: a genuine access token past its expiry; the client
	// refreshes.
	CodeTokenExpired apperr.Code = "AUTH_TOKEN_EXPIRED"
	// CodeSessionInactive: the session does not exist, was revoked or
	// expired, or its user or device was deleted.
	CodeSessionInactive apperr.Code = "AUTH_SESSION_NOT_FOUND_OR_INACTIVE"
	// CodeSessionDeviceMismatch: the request came from another device than
	// the session's; the session is revoked (AUTH-004).
	CodeSessionDeviceMismatch apperr.Code = "AUTH_SESSION_NOT_MATCHING_CURRENT_DEVICE"
	// CodeRefreshTokenInvalid: an unknown or malformed refresh token.
	CodeRefreshTokenInvalid apperr.Code = "AUTH_REFRESH_TOKEN_INVALID"
	// CodeRefreshTokenExpired: the refresh token passed its idle limit or
	// the session its absolute limit; the session is closed.
	CodeRefreshTokenExpired apperr.Code = "AUTH_REFRESH_TOKEN_EXPIRED"
	// CodeRefreshTokenReused: a rotated refresh token came back; the whole
	// session (token family) is revoked (K12).
	CodeRefreshTokenReused apperr.Code = "AUTH_REFRESH_TOKEN_REUSED"
	// CodeSessionNotFound: the caller's own session list has no such
	// session; another user's session looks the same (AUTH-016).
	CodeSessionNotFound apperr.Code = "AUTH_SESSION_NOT_FOUND"
	// CodePasswordPolicy: a new password breaks the rule of AUTH-009.
	CodePasswordPolicy apperr.Code = "PASSWORD_POLICY_VIOLATION"
	// CodeStaffLoginFailed: wrong password, unknown, disabled or locked
	// staff account; all four are indistinguishable (AUTH-010, AUTH-011).
	CodeStaffLoginFailed apperr.Code = "STAFF_LOGIN_FAILED"
	// CodeStaffSessionEnded: a staff token whose session was ended by
	// logout, password change or disabling (token_version moved).
	CodeStaffSessionEnded apperr.Code = "STAFF_SESSION_ENDED"
	// CodeStaffPasswordChangeRequired: the account must change its
	// password before anything else (AUTH-014).
	CodeStaffPasswordChangeRequired apperr.Code = "STAFF_PASSWORD_CHANGE_REQUIRED"
	// CodeStaffCurrentPasswordMismatch: a password change with a wrong
	// current password (AUTH-013).
	CodeStaffCurrentPasswordMismatch apperr.Code = "STAFF_CURRENT_PASSWORD_MISMATCH"
)

// Codes lists every code this package returns, for the contract test.
func Codes() []apperr.Code {
	return []apperr.Code{
		CodeEmailOrPasswordMismatch, CodeAccountSuspended, CodeTokenMissing, CodeTokenInvalid,
		CodeTokenExpired, CodeSessionInactive, CodeSessionDeviceMismatch, CodeRefreshTokenInvalid,
		CodeRefreshTokenExpired, CodeRefreshTokenReused, CodeSessionNotFound, CodePasswordPolicy,
		CodeStaffLoginFailed, CodeStaffSessionEnded, CodeStaffPasswordChangeRequired,
		CodeStaffCurrentPasswordMismatch,
	}
}

// Lifetimes (ADR-0025). Access tokens are short (K12); a refresh token's
// idle limit slides with each rotation; the session's absolute limit does
// not move.
const (
	AccessTokenTTL = 15 * time.Minute

	// Marketplace: Java's 30-day sliding window, now with a 90-day ceiling.
	UserRefreshIdleTTL = 30 * 24 * time.Hour
	UserSessionMaxTTL  = 90 * 24 * time.Hour

	// Staff: ASVS 5.0 V7.3 (L2): re-authentication after 30 minutes idle
	// and at least every 12 hours; Java's staff refresh lived 12 hours.
	StaffRefreshIdleTTL = 30 * time.Minute
	StaffSessionMaxTTL  = 12 * time.Hour

	// touchInterval bounds how often a request writes last_seen_at.
	touchInterval = 5 * time.Minute
)

// Staff lockout (AUTH-011): StaffLockThreshold consecutive wrong passwords
// lock the account for StaffLockDuration. While locked no attempt, right
// or wrong, changes the counter or the lock.
const (
	StaffLockThreshold = 5
	StaffLockDuration  = 15 * time.Minute
)

// Password rule (AUTH-009): at least 12 characters, at most 72 bytes of
// UTF-8, no composition rule. Sign-in accepts whatever the account has.
const (
	PasswordMinChars = 12
	PasswordMaxBytes = 72
)

// User and staff statuses (CHECK constraints of 0020 and 0021).
const (
	UserActive     = "ACTIVE"
	UserSuspended  = "SUSPENDED"
	StaffActive    = "ACTIVE"
	StaffDisabled  = "DISABLED"
	principalUser  = "USER"
	principalStaff = "STAFF"
)

// Revoke reasons (user_sessions and staff_sessions CHECK constraints).
const (
	reasonLogout         = "LOGOUT"
	reasonDeviceMismatch = "DEVICE_MISMATCH"
	reasonRefreshReused  = "REFRESH_REUSED"
	reasonExpired        = "EXPIRED"
	reasonInactive       = "INACTIVE"
)

// deviceKeyPattern is the X-Device-Id format (ADR-0021 §2) and the
// devices.device_key CHECK.
var deviceKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

// ValidDeviceKey reports whether k is a well-formed device id.
func ValidDeviceKey(k string) bool { return deviceKeyPattern.MatchString(k) }

// CheckPassword applies the password rule to a new password. It is the
// rule's one place; every surface that sets a password calls it.
func CheckPassword(p string) error {
	if utf8.RuneCountInString(p) < PasswordMinChars || len(p) > PasswordMaxBytes {
		return apperr.New(apperr.KindInvalid, CodePasswordPolicy)
	}
	return nil
}

// UserPrincipal is an authenticated marketplace caller.
type UserPrincipal struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
	DeviceKey string
}

// StaffPrincipal is an authenticated staff caller.
type StaffPrincipal struct {
	StaffID            uuid.UUID
	SessionID          uuid.UUID
	MustChangePassword bool
	// MFA reports a session whose second factor was verified. Always
	// false until TOTP lands (K13, Faz 8).
	MFA bool
}

// TokenPair is what a sign-in or refresh hands out.
type TokenPair struct {
	AccessToken           string
	AccessTokenExpiresAt  time.Time
	RefreshToken          string
	RefreshTokenExpiresAt time.Time
	SessionID             uuid.UUID
}

// StaffTokenPair is a staff sign-in or refresh.
type StaffTokenPair struct {
	TokenPair
	MustChangePassword bool
}

// Session is one open session as its owner sees it.
type Session struct {
	ID         uuid.UUID
	Platform   string // IOS, ANDROID, WEB or ""
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	Current    bool
}

// User is the caller's own identity core.
type User struct {
	ID        uuid.UUID
	Email     string
	Status    string
	CreatedAt time.Time
}

// StaffMember is the staff caller's own account.
type StaffMember struct {
	ID                 uuid.UUID
	Email              string
	MustChangePassword bool
}

// staffFailure applies one wrong password to the counter: at the
// threshold the account locks and the counter starts over.
func staffFailure(failures int32, now time.Time) (int32, *time.Time) {
	failures++
	if failures >= StaffLockThreshold {
		until := now.Add(StaffLockDuration)
		return 0, &until
	}
	return failures, nil
}

// staffLocked reports whether a lock is in force at now.
func staffLocked(lockedUntil *time.Time, now time.Time) bool {
	return lockedUntil != nil && now.Before(*lockedUntil)
}

// refreshExpiry is a new refresh token's limit: the idle TTL from now,
// never past the session's absolute end.
func refreshExpiry(now, sessionEnd time.Time, idle time.Duration) time.Time {
	if e := now.Add(idle); e.Before(sessionEnd) {
		return e
	}
	return sessionEnd
}
