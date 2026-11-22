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
	"net/netip"
	"regexp"
	"strings"
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
	CodeEmailOrPasswordMismatch apperr.Code = "AUTH_EMAIL_OR_PASSWORD_MISMATCH" //nolint:gosec // G101: an error code, not a credential
	// CodeAccountSuspended: the password was right but the account is
	// suspended. Only someone who knows the password learns it.
	CodeAccountSuspended apperr.Code = "AUTH_ACCOUNT_SUSPENDED"
	// CodeTokenMissing: a protected operation without a bearer token.
	CodeTokenMissing apperr.Code = "AUTH_TOKEN_MISSING" //nolint:gosec // G101: an error code, not a credential
	// CodeTokenInvalid: a malformed, forged, foreign-class or not yet valid
	// access token.
	CodeTokenInvalid apperr.Code = "AUTH_TOKEN_INVALID" //nolint:gosec // G101: an error code, not a credential
	// CodeTokenExpired: a genuine access token past its expiry; the client
	// refreshes.
	CodeTokenExpired apperr.Code = "AUTH_TOKEN_EXPIRED" //nolint:gosec // G101: an error code, not a credential
	// CodeSessionInactive: the session does not exist, was revoked or
	// expired, or its user or device was deleted.
	CodeSessionInactive apperr.Code = "AUTH_SESSION_NOT_FOUND_OR_INACTIVE"
	// CodeSessionDeviceMismatch: the request came from another device than
	// the session's; the session is revoked (AUTH-004).
	CodeSessionDeviceMismatch apperr.Code = "AUTH_SESSION_NOT_MATCHING_CURRENT_DEVICE"
	// CodeRefreshTokenInvalid: an unknown or malformed refresh token.
	CodeRefreshTokenInvalid apperr.Code = "AUTH_REFRESH_TOKEN_INVALID" //nolint:gosec // G101: an error code, not a credential
	// CodeRefreshTokenExpired: the refresh token passed its idle limit or
	// the session its absolute limit; the session is closed.
	CodeRefreshTokenExpired apperr.Code = "AUTH_REFRESH_TOKEN_EXPIRED" //nolint:gosec // G101: an error code, not a credential
	// CodeRefreshTokenReused: a rotated refresh token came back; the whole
	// session (token family) is revoked (K12).
	CodeRefreshTokenReused apperr.Code = "AUTH_REFRESH_TOKEN_REUSED" //nolint:gosec // G101: an error code, not a credential
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

	// The account codes (ADR-0029 Karar 2 and 4). The names avoid the
	// words gosec's G101 reads as credentials; the values are the
	// contract's.

	// CodeRegistrationInvalid: a registration link or registration token
	// that is unknown, revoked, expired or consumed; the four are
	// indistinguishable.
	CodeRegistrationInvalid apperr.Code = "REGISTRATION_TOKEN_INVALID"
	// CodeConsentRequired: a mandatory agreement was not consented to; the
	// registration token is not read (USR-005).
	CodeConsentRequired apperr.Code = "CONSENT_REQUIRED"
	// CodeResetLinkInvalid: a reset token that is unknown, expired,
	// revoked or used; the four are indistinguishable (USR-016).
	CodeResetLinkInvalid apperr.Code = "PASSWORD_RESET_TOKEN_INVALID"
	// CodeCurrentMismatch: an in-app change with a wrong current password.
	CodeCurrentMismatch apperr.Code = "PASSWORD_MISMATCH"
	// CodeNewEqualsCurrent: the new password is the current one.
	CodeNewEqualsCurrent apperr.Code = "PASSWORD_REUSED"
	// CodeConfirmationMismatch: the new password and its confirmation
	// differ.
	CodeConfirmationMismatch apperr.Code = "PASSWORD_CONFIRMATION_MISMATCH"
	// CodeVerificationTargetInUse: the registration's address got an
	// account between the first and the third step, or a verified address
	// or number is another active account's. A verification tells only the
	// caller who entered the right code (ADR-0033).
	CodeVerificationTargetInUse apperr.Code = "VERIFICATION_TARGET_IN_USE"

	// The verification codes (ADR-0029 Karar 3, ADR-0033).

	// CodeVerificationNotFound: the caller has no such verification;
	// another user's, a burned and a canceled one look the same (USR-011).
	CodeVerificationNotFound apperr.Code = "VERIFICATION_NOT_FOUND"
	// CodeVerificationWrong: a wrong code; one attempt is spent.
	CodeVerificationWrong apperr.Code = "VERIFICATION_CODE_INVALID"
	// CodeVerificationExpired: the ten minutes are over; checked before the
	// code, so a right and a wrong code get the same answer (USR-012).
	CodeVerificationExpired apperr.Code = "VERIFICATION_CODE_EXPIRED"
	// CodeVerificationExhausted: the verification burned after five wrong
	// codes; even the right one is refused (USR-009).
	CodeVerificationExhausted apperr.Code = "VERIFICATION_ATTEMPTS_EXHAUSTED"
	// CodeVerificationConfirmed: a second confirmation, refused whatever
	// the code (USR-013).
	CodeVerificationConfirmed apperr.Code = "VERIFICATION_ALREADY_CONFIRMED"
	// CodeVerificationResendTooSoon: the resend cooldown, the target's
	// hourly budget or the caller's daily cap holds (USR-010); the answer
	// carries Retry-After.
	CodeVerificationResendTooSoon apperr.Code = "VERIFICATION_RESEND_TOO_SOON"
	// CodeCursorInvalid: a list cursor that is malformed or belongs to
	// another query.
	CodeCursorInvalid apperr.Code = "CURSOR_INVALID"
)

// Codes lists every code this package returns, for the contract test.
func Codes() []apperr.Code {
	return []apperr.Code{
		CodeEmailOrPasswordMismatch, CodeAccountSuspended, CodeTokenMissing, CodeTokenInvalid,
		CodeTokenExpired, CodeSessionInactive, CodeSessionDeviceMismatch, CodeRefreshTokenInvalid,
		CodeRefreshTokenExpired, CodeRefreshTokenReused, CodeSessionNotFound, CodePasswordPolicy,
		CodeStaffLoginFailed, CodeStaffSessionEnded, CodeStaffPasswordChangeRequired,
		CodeStaffCurrentPasswordMismatch, CodeRegistrationInvalid, CodeConsentRequired, CodeResetLinkInvalid,
		CodeCurrentMismatch, CodeNewEqualsCurrent, CodeConfirmationMismatch, CodeVerificationTargetInUse,
		CodeVerificationNotFound, CodeVerificationWrong, CodeVerificationExpired, CodeVerificationExhausted,
		CodeVerificationConfirmed, CodeVerificationResendTooSoon, CodeCursorInvalid,
	}
}

// Verification policy (ADR-0029 Karar 3, ADR-0033).
const (
	// VerificationTTL is a code's life, from the start of the
	// verification (NIST SP 800-63B Rev. 3 §5.1.3.2).
	VerificationTTL = 10 * time.Minute
	// VerificationAttempts is how many wrong codes a verification takes;
	// the last one burns it (USR-009).
	VerificationAttempts = 5
	// VerificationCooldown and VerificationsPerTargetHour bound the codes
	// one target gets, from every user together: none within the cooldown
	// of the last, at most VerificationsPerTargetHour an hour (USR-010).
	VerificationCooldown       = time.Minute
	VerificationsPerTargetHour = 5
	// TextsPerUserDay and MailCodesPerUserDay are a user's daily cap of
	// verifications per channel, over every target: the toll fraud bound
	// of one account (ADR-0033).
	TextsPerUserDay     = 10
	MailCodesPerUserDay = 20
	// DefaultTextsPerHour is the global hourly cap of codes sent by text
	// message (KOALA_SMS_PER_HOUR overrides it): what a spread-out abuse
	// can cost in an hour. A held code is counted and alerted on.
	DefaultTextsPerHour = 200
	// VerificationRetention is how long a verification is kept before the
	// purge hard-deletes it (M2): the longest budget window and a margin.
	VerificationRetention = 48 * time.Hour
)

// Verification channels (user_verifications CHECK) and the statuses a
// caller sees (the contract's VerificationStatus). The contract also has
// a burned and a canceled status, but a burned or canceled verification
// reads as missing (USR-011), so no answer carries them (ADR-0033).
const (
	ChannelEmail = "EMAIL"
	ChannelPhone = "PHONE"

	StatusPending   = "PENDING"
	StatusConfirmed = "CONFIRMED"
	StatusExpired   = "EXPIRED"
)

// Verification is one of the caller's verifications as the caller sees
// it: never the code, and the target only masked.
type Verification struct {
	ID                uuid.UUID
	Channel           string
	Status            string
	MaskedTarget      string
	RemainingAttempts int32
	CreatedAt         time.Time
	ExpiresAt         time.Time
	ConfirmedAt       *time.Time
}

// verificationStatus is the status at now of a verification its owner
// can see: neither burned nor canceled, which read as missing (USR-011).
// An open one past its end is expired.
func verificationStatus(confirmed *time.Time, expires, now time.Time) string {
	switch {
	case confirmed != nil:
		return StatusConfirmed
	case !now.Before(expires):
		return StatusExpired
	default:
		return StatusPending
	}
}

// NormalizePhone turns a phone number as people type it into E.164: the
// separators (space, '-', '.', '(', ')') go, an international "00"
// becomes '+', and a Turkish number without its country code (05XX… or
// 5XX…, ten digits after the 0) gets +90. ok is false for anything that
// is not then a well-formed E.164 number. Whether the number may get a
// code is TextableNumber's decision.
func NormalizePhone(raw string) (string, bool) {
	var b strings.Builder
	for i, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')' || r == '\u00a0':
		default:
			return "", false
		}
	}
	n := b.String()
	switch {
	case strings.HasPrefix(n, "+"):
	case strings.HasPrefix(n, "00"):
		n = "+" + n[2:]
	case len(n) == 11 && strings.HasPrefix(n, "0"):
		n = "+90" + n[1:]
	case len(n) == 10 && strings.HasPrefix(n, "5"):
		n = "+90" + n
	case len(n) == 12 && strings.HasPrefix(n, "90"):
		n = "+" + n
	default:
		return "", false
	}
	if !e164Pattern.MatchString(n) {
		return "", false
	}
	return n, true
}

// e164Pattern is an E.164 number (users_phone_check).
var e164Pattern = regexp.MustCompile(`^\+[1-9]\d{7,14}$`)

// textableCountries are the country codes a code may be texted to, with
// the national number's length and the first digit of a mobile number
// there. Today only Türkiye's mobile numbers (+90 5XX XXX XX XX): every
// user is in Türkiye, and an allowlist of mobile ranges is the standard
// defense against SMS pumping (toll fraud), which lives on premium and
// international numbers (ADR-0033). A new country is a new entry.
func textableCountries() map[string]struct {
	digits int
	mobile byte
} {
	return map[string]struct {
		digits int
		mobile byte
	}{"+90": {digits: 10, mobile: '5'}}
}

// TextableNumber reports whether an E.164 number may get a code by text
// message: a mobile number of an allowed country.
func TextableNumber(e164 string) bool {
	for cc, rule := range textableCountries() {
		if n, ok := strings.CutPrefix(e164, cc); ok && len(n) == rule.digits && n[0] == rule.mobile {
			return true
		}
	}
	return false
}

// maskTarget is what the caller sees of a target: the first character of
// an address's local part and its domain (a***@example.com), a number's
// country prefix and last four digits (+90******4567).
func maskTarget(channel, target string) string {
	if channel == ChannelPhone {
		if len(target) <= 7 {
			return strings.Repeat("*", len(target))
		}
		return target[:3] + strings.Repeat("*", len(target)-7) + target[len(target)-4:]
	}
	at := strings.LastIndexByte(target, '@')
	if at <= 0 {
		return "***"
	}
	first, _ := utf8.DecodeRuneInString(target)
	return string(first) + "***" + target[at:]
}

// Account flows (ADR-0029 Karar 2 and 4, ADR-0032).
const (
	// RegistrationLinkTTL is the life of the link mailed at the first
	// step, from the moment it is minted (USR-003).
	RegistrationLinkTTL = time.Hour
	// RegistrationTokenTTL is the life of the token the second step hands
	// out for the third (USR-004).
	RegistrationTokenTTL = 15 * time.Minute
	// ResetLinkTTL is the life of a password reset link.
	ResetLinkTTL = 15 * time.Minute
	// LinkMailCooldown and LinkMailsPerHour bound the link mails one
	// address (registration) or one account (reset) gets: none within the
	// cooldown of the last, at most LinkMailsPerHour an hour. Karar 3's
	// resend rule for OTP, applied to links (ADR-0032); a request over the
	// budget is answered like any other and sends nothing.
	LinkMailCooldown = time.Minute
	LinkMailsPerHour = 5
	// DefaultRegistrationMailsPerHour is the global cap of registration
	// requests handled an hour, over every address, and so of registration
	// link mails (KOALA_REGISTRATION_MAILS_PER_HOUR overrides it). It
	// bounds what a spread-out flood can send through the relay; every
	// request spends it alike, registered address or not, and a held
	// request is counted and alerted on (ADR-0032 Karar 6).
	DefaultRegistrationMailsPerHour = 500
	// AccountRowRetention is how long pending registrations and reset
	// requests are kept before the purge hard-deletes them (M2; USR-007).
	AccountRowRetention = 30 * 24 * time.Hour
	// RegistrationLedgerRetention is how long the global cap's ledger
	// keeps a request: its hour and a margin (M2).
	RegistrationLedgerRetention = 24 * time.Hour
)

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

// Staff lockout (AUTH-011, ADR-0025 §6; PCI DSS 4.0 8.3.4, OWASP):
//   - per (account, client address): StaffLockThreshold consecutive wrong
//     passwords from the address lock the account for that address for
//     StaffLockDuration;
//   - per account, from any address: from StaffBackoffAfter consecutive
//     wrong passwords on, each one sets a backoff that doubles from
//     StaffBackoffBase up to StaffBackoffMax. The backoff holds back only
//     unknown sources: an address the member signed in from within
//     KnownSourceWindow ("known-good source") is exempt, so failures from
//     elsewhere, however many, cannot keep the member out of a place they
//     use. From a new address the backoff can be renewed for as long as
//     the failures go on. The address lock applies to every address,
//     known or not.
//
// While a lock or backoff is in force for the address every attempt is
// refused unexamined (verified against the dummy hash) and changes
// nothing; the answer is the plain failure.
const (
	StaffLockThreshold = 5
	StaffLockDuration  = 30 * time.Minute
	StaffBackoffAfter  = 10
	StaffBackoffBase   = time.Second
	StaffBackoffMax    = 15 * time.Minute
)

// KnownSourceWindow is how long a successful sign-in makes its source a
// known-good one (ADR-0025 §5–6): a staff member's (account, address)
// pair, a user's device. A known source skips the account-wide
// protections an attacker elsewhere can trigger (the staff backoff; the
// marketplace lock and account-wide rate bucket) but not the ones of its
// own address.
const KnownSourceWindow = 30 * 24 * time.Hour

// source is a staff member's state at one client address.
type source struct {
	failures    int32
	lockedUntil *time.Time
	lastSuccess *time.Time
	// successVersion is the account's token version at lastSuccess.
	successVersion *int64
}

// locked reports whether the address lock holds at now.
func (s source) locked(now time.Time) bool { return inForce(s.lockedUntil, now) }

// known reports whether the address is a known-good source of an account
// whose token version is version: the member signed in from it within
// KnownSourceWindow before now, and no logout, password change or
// disabling moved the version since (ADR-0025 §6).
func (s source) known(now time.Time, version int64) bool {
	return s.lastSuccess != nil && now.Sub(*s.lastSuccess) < KnownSourceWindow &&
		s.successVersion != nil && *s.successVersion == version
}

// blocked reports whether a sign-in from s is refused unexamined at now:
// its address is locked, or the account backoff holds and s is not a
// known-good source.
func (s source) blocked(backoffUntil *time.Time, now time.Time, version int64) bool {
	return s.locked(now) || (inForce(backoffUntil, now) && !s.known(now, version))
}

// Marketplace lock (ADR-0025 §5; NIST SP 800-63B 5.2.2): every
// UserLockThreshold consecutive failures on a sign-in target lock it for
// UserLockDuration. The counter is the attempt log itself. A locked target
// answers exactly like a wrong password, except to a device the account
// signed in on within KnownSourceWindow, which the lock does not hold
// back.
const (
	UserLockThreshold = 100
	UserLockDuration  = time.Hour
)

// RefreshGraceWindow is how long after a rotation the same refresh token,
// from the same device, still gets the successor already issued instead of
// counting as reuse (ADR-0025 §3): a client retry, not a theft.
const RefreshGraceWindow = 10 * time.Second

// Password rule (AUTH-009, ADR-0025 §5): at least 15 and at most 128
// characters, no composition rule. NIST SP 800-63B-4 asks for 15 when the
// password is the only factor, which it is for users and, until TOTP
// (Faz 8), for staff; it asks to allow at least 64, and the ceiling also
// bounds the hashing work. Sign-in accepts whatever the account has.
const (
	PasswordMinChars = 15
	PasswordMaxChars = 128
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
	reasonLocked         = "LOCKED"
	// reasonReset ends every session after a password reset, reasonChanged
	// every other session after an in-app change (ADR-0029 Karar 4).
	reasonReset   = "PASSWORD_RESET"
	reasonChanged = "PASSWORD_CHANGED"
)

// Attempt outcomes (login_attempts CHECK constraint).
const (
	outcomeSucceeded = "SUCCEEDED"
	outcomeFailed    = "FAILED"
	outcomeLocked    = "LOCKED"
)

// deviceKeyPattern is the X-Device-Id format (ADR-0021 §2) and the
// devices.device_key CHECK.
var deviceKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

// ValidDeviceKey reports whether k is a well-formed device id.
func ValidDeviceKey(k string) bool { return deviceKeyPattern.MatchString(k) }

// CheckPassword applies the password rule to a new password. It is the
// rule's one place; every surface that sets a password calls it.
func CheckPassword(p string) error {
	if n := utf8.RuneCountInString(p); n < PasswordMinChars || n > PasswordMaxChars {
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

// addressFailure applies one wrong password to an (account, address)
// counter: at the threshold the pair locks and the counter starts over.
func addressFailure(failures int32, now time.Time) (int32, *time.Time) {
	failures++
	if failures >= StaffLockThreshold {
		until := now.Add(StaffLockDuration)
		return 0, &until
	}
	return failures, nil
}

// accountFailure applies one wrong password to the account-wide counter
// and returns the backoff it sets, if any.
func accountFailure(failures int32, now time.Time) (int32, *time.Time) {
	failures++
	if failures < StaffBackoffAfter {
		return failures, nil
	}
	delay := StaffBackoffMax
	if exp := failures - StaffBackoffAfter; exp < 20 { // 2^20 s is far past the cap
		delay = min(StaffBackoffBase<<exp, StaffBackoffMax)
	}
	until := now.Add(delay)
	return failures, &until
}

// inForce reports whether a lock or backoff ending at until holds at now.
func inForce(until *time.Time, now time.Time) bool {
	return until != nil && now.Before(*until)
}

// mailboxKey is the mailbox an address delivers to, for the mail budget
// and the rate limit keys of the account requests (ADR-0032 Karar 6): the
// sub-address after '+' is dropped, googlemail.com is gmail.com, and at
// Gmail the dots of the local part are dropped. The stored address does
// not change. email is normalized (normalizeEmail).
func mailboxKey(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return email
	}
	local, domain := email[:at], email[at+1:]
	if plus := strings.IndexByte(local, '+'); plus >= 0 {
		local = local[:plus]
	}
	if domain == "googlemail.com" {
		domain = "gmail.com"
	}
	if domain == "gmail.com" {
		local = strings.ReplaceAll(local, ".", "")
	}
	return local + "@" + domain
}

// ClientKey is the address part of a lock or rate limit key: the address,
// an IPv6 client by its /64 (one host usually owns a whole /64).
func ClientKey(a netip.Addr) string {
	if !a.IsValid() {
		return "unknown"
	}
	a = a.Unmap()
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
}

// refreshExpiry is a new refresh token's limit: the idle TTL from now,
// never past the session's absolute end.
func refreshExpiry(now, sessionEnd time.Time, idle time.Duration) time.Time {
	if e := now.Add(idle); e.Before(sessionEnd) {
		return e
	}
	return sessionEnd
}
