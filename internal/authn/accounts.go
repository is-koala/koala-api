package authn

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/mail"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/mailer"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// mailTemplates are the account mails (ADR-0032).
//
//go:embed mail/*.txt mail/*.html
var mailTemplates embed.FS

// The account mails and the web paths their links open (Java's
// AuthLinkManager paths, so the web client keeps its routes).
const (
	mailRegistrationLink = "registration_link"
	mailResetLink        = "reset_link"
	mailChanged          = "password_changed"
	mailWelcome          = "welcome"

	registrationLinkPath = "hesap-dogrula"
	resetLinkPath        = "sifre-sifirla"
)

// jobQueue inserts a River job in the caller's transaction (ADR-0010): a
// mail is a guaranteed side effect, so it commits with the data or not at
// all.
type jobQueue interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// mailSender delivers one message; internal/integrations/smtp is the real
// one. It is called from River jobs only, outside any transaction.
type mailSender interface {
	Send(ctx context.Context, m mailer.Message) error
}

// Accounts runs the account flows of marketplace users (ADR-0029 Karar 2
// and 4, ADR-0032): three-step registration, password reset and the
// in-app password change. It shares the sessions, tokens and password
// hashing of the authentication service (ADR-0025).
//
// The two requests that take an address, the start of a registration and
// a reset request, only queue a job: the request does the same work for a
// registered and an unknown address, and the job decides. Tokens that
// travel by mail are minted when the mail is sent, so no token is ever
// stored in plain text, in the job queue included.
type Accounts struct {
	users  *Service
	queue  jobQueue
	sender mailSender
	mails  *mailer.Templates
	web    *url.URL
	// registrationsPerHour is the global hourly cap of registration mails.
	registrationsPerHour int64
}

// NewAccounts returns the account flows on users' database and clock.
// webBaseURL is the web origin the mailed links open; registrationsPerHour
// is the global hourly cap of registration mails, 0 for
// DefaultRegistrationMailsPerHour.
func NewAccounts(users *Service, queue jobQueue, sender mailSender, webBaseURL string, registrationsPerHour int) (*Accounts, error) {
	if users == nil || queue == nil || sender == nil {
		return nil, errors.New("authn: accounts need the user service, a job queue and a mail sender")
	}
	web, err := url.Parse(webBaseURL)
	if err != nil || (web.Scheme != "https" && web.Scheme != "http") || web.Host == "" {
		return nil, errors.New("authn: the web base URL must be an absolute http or https URL")
	}
	mails, err := parseMailTemplates()
	if err != nil {
		return nil, err
	}
	limit := int64(DefaultRegistrationMailsPerHour)
	if registrationsPerHour > 0 {
		limit = int64(registrationsPerHour)
	}
	return &Accounts{users: users, queue: queue, sender: sender, mails: mails, web: web, registrationsPerHour: limit}, nil
}

// parseMailTemplates reads the account mails and checks that each has its
// subject, text and HTML.
func parseMailTemplates() (*mailer.Templates, error) {
	sub, err := fs.Sub(mailTemplates, "mail")
	if err != nil {
		return nil, fmt.Errorf("authn: mail templates: %w", err)
	}
	mails, err := mailer.ParseTemplates(sub)
	if err != nil {
		return nil, fmt.Errorf("authn: mail templates: %w", err)
	}
	if err := mails.Check(mailRegistrationLink, mailResetLink, mailChanged, mailWelcome); err != nil {
		return nil, fmt.Errorf("authn: mail templates: %w", err)
	}
	return mails, nil
}

// RegistrationRequest is the first step of a registration.
type RegistrationRequest struct {
	Email     string
	FirstName string
	LastName  string
	Employer  bool
}

// StartRegistration queues the first step of a registration (USR-001).
// The answer is the same for every well-formed address and costs the same:
// one job in one transaction. HandleRegistrationRequest does the rest.
func (a *Accounts) StartRegistration(ctx context.Context, in RegistrationRequest) error {
	email, err := checkEmail(in.Email)
	if err != nil {
		return err
	}
	first, last := strings.TrimSpace(in.FirstName), strings.TrimSpace(in.LastName)
	if first == "" {
		return invalidField("/firstName")
	}
	if last == "" {
		return invalidField("/lastName")
	}
	return a.enqueue(ctx, registrationRequestedArgs{Email: email, FirstName: first, LastName: last, Employer: in.Employer})
}

// HandleRegistrationRequest is the job of a registration start. A
// registered address gets nothing: no pending registration, no mail
// (USR-001). Otherwise the address's open registration is revoked and a
// new one written (USR-002), and its link mail queued in the same
// transaction, unless the mailbox's budget or the global hourly cap is
// spent (ADR-0032 Karar 6); a held mail is counted, never reported to the
// requester.
func (a *Accounts) HandleRegistrationRequest(ctx context.Context, in RegistrationRequest) error {
	now := a.users.clock.Now()
	err := a.users.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		registered, err := q.EmailRegistered(ctx, in.Email)
		if err != nil {
			return fmt.Errorf("authn: look up the address: %w", err)
		}
		if registered {
			return nil
		}
		mailbox := mailboxKey(in.Email)
		recent, err := q.CountRecentRegistrationMails(ctx, authndb.CountRecentRegistrationMailsParams{
			MailboxKey: mailbox, WindowStart: now.Add(-time.Hour), CooldownStart: now.Add(-LinkMailCooldown),
		})
		if err != nil {
			return fmt.Errorf("authn: count registration mails: %w", err)
		}
		switch {
		case recent.InCooldown > 0 || recent.InWindow >= LinkMailsPerHour:
			a.users.metrics.mailHeld(heldMailbox)
			a.users.logger.InfoContext(ctx, "registration mail held back by the mailbox's budget")
			return nil
		case recent.Everyone >= a.registrationsPerHour:
			a.users.metrics.mailHeld(heldGlobal)
			a.users.logger.WarnContext(ctx, "registration mail held back by the global hourly cap")
			return nil
		}
		err = q.RevokeOpenPendingRegistrations(ctx, authndb.RevokeOpenPendingRegistrationsParams{Now: now, Email: in.Email})
		if err != nil {
			return fmt.Errorf("authn: revoke the open registration: %w", err)
		}
		id := newID()
		err = q.InsertPendingRegistration(ctx, authndb.InsertPendingRegistrationParams{
			ID: id, Email: &in.Email, MailboxKey: mailbox, FirstName: &in.FirstName, LastName: &in.LastName,
			Employer: in.Employer, CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("authn: insert the pending registration: %w", err)
		}
		return a.enqueueTx(ctx, tx, authMailArgs{Mail: mailRegistrationLink, Ref: id})
	})
	if err != nil {
		return fmt.Errorf("authn: registration request: %w", err)
	}
	return nil
}

// RegistrationGrant is what the second step hands out: the token the
// third step takes, its end and the pending registration's employer flag.
type RegistrationGrant struct {
	Token     string
	ExpiresAt time.Time
	Employer  bool
}

// VerifyRegistration turns a link token into a registration token
// (USR-004). The link token is not consumed, so the link can be opened
// twice; each verification replaces the registration token the last one
// handed out. An unknown, revoked, consumed and expired link are the same
// error, and an expired one revokes its registration (USR-003).
func (a *Accounts) VerifyRegistration(ctx context.Context, linkToken string) (RegistrationGrant, error) {
	invalid := apperr.New(apperr.KindInvalid, CodeRegistrationInvalid)
	if !wellFormedOpaque(linkToken) {
		return RegistrationGrant{}, invalid
	}
	now := a.users.clock.Now()
	var grant RegistrationGrant
	var outcome error
	err := a.users.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		row, err := q.GetPendingRegistrationByLinkForUpdate(ctx, hashOpaque(linkToken))
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = invalid
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: load the pending registration: %w", err)
		}
		if row.ConsumedAt != nil || row.RevokedAt != nil {
			outcome = invalid
			return nil
		}
		if row.LinkExpiresAt == nil || !now.Before(*row.LinkExpiresAt) {
			// Expired: the registration ends here, and the revocation
			// commits although the request fails.
			outcome = invalid
			if err := q.RevokePendingRegistration(ctx, authndb.RevokePendingRegistrationParams{Now: now, ID: row.ID}); err != nil {
				return fmt.Errorf("authn: revoke the expired registration: %w", err)
			}
			return nil
		}
		token := newOpaque()
		expires := now.Add(RegistrationTokenTTL)
		err = q.SetRegistrationToken(ctx, authndb.SetRegistrationTokenParams{TokenHash: hashOpaque(token), ExpiresAt: expires, ID: row.ID})
		if err != nil {
			return fmt.Errorf("authn: store the registration token: %w", err)
		}
		grant = RegistrationGrant{Token: token, ExpiresAt: expires, Employer: row.Employer}
		return nil
	})
	if err != nil {
		return RegistrationGrant{}, fmt.Errorf("authn: verify registration: %w", err)
	}
	if outcome != nil {
		return RegistrationGrant{}, outcome
	}
	return grant, nil
}

// ConsentDecision is one agreement version consented to or declined.
type ConsentDecision struct {
	DocumentCode string
	Version      string
	Granted      bool
}

// RegistrationCompletion is the third step: the registration token, the
// password, the consents, the device the session opens on, and the client
// address and User-Agent the consents are recorded with (ADR-0032 Karar 8).
type RegistrationCompletion struct {
	Token     string
	Password  string
	Consents  []ConsentDecision
	DeviceKey string
	Platform  string
	ClientIP  netip.Addr
	UserAgent string
}

// maxUserAgentBytes bounds the User-Agent a consent record keeps (Java's
// VARCHAR(512)).
const maxUserAgentBytes = 512

// CompleteRegistration creates the user from the pending registration and
// signs it in on the request's device (USR-006). The consents are checked
// first, before the token is read: a missing mandatory consent neither
// consumes the token nor says whether it is valid (USR-005). The token is
// consumed by one conditional statement in the transaction that writes
// the user, the consents, the session and the welcome mail, so it works
// once (USR-004).
func (a *Accounts) CompleteRegistration(ctx context.Context, in RegistrationCompletion) (TokenPair, error) {
	if err := checkDeviceKey(in.DeviceKey); err != nil {
		return TokenPair{}, err
	}
	if err := a.checkConsents(ctx, in.Consents); err != nil {
		return TokenPair{}, err
	}
	if err := CheckPassword(in.Password); err != nil {
		return TokenPair{}, err
	}
	invalid := apperr.New(apperr.KindInvalid, CodeRegistrationInvalid)
	if !wellFormedOpaque(in.Token) {
		return TokenPair{}, invalid
	}
	digest := hashOpaque(in.Token)
	now := a.users.clock.Now()
	// The hash costs tens of milliseconds; a token that cannot work does
	// not get it.
	_, err := a.users.reads.PeekRegistrationToken(ctx, authndb.PeekRegistrationTokenParams{TokenHash: digest, Now: now})
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenPair{}, invalid
	}
	if err != nil {
		return TokenPair{}, fmt.Errorf("authn: look up the registration token: %w", err)
	}
	hash, err := a.users.passwords.Hash(ctx, in.Password)
	if err != nil {
		return TokenPair{}, fmt.Errorf("authn: hash password: %w", err)
	}
	var pair TokenPair
	var outcome error
	err = a.users.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		p, err := q.ConsumePendingRegistration(ctx, authndb.ConsumePendingRegistrationParams{Now: now, TokenHash: digest})
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = invalid // used or expired since the look-up
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: consume the pending registration: %w", err)
		}
		userID := newID()
		err = q.InsertUser(ctx, authndb.InsertUserParams{
			ID: userID, Email: p.Email, PasswordHash: &hash, Status: UserActive,
			FirstName: p.FirstName, LastName: p.LastName, Employer: p.Employer, EmailVerified: true, CreatedAt: now,
		})
		if emailTaken(err) {
			// The address got an account after the first step. The
			// error rolls the consumption back with the rest.
			return apperr.New(apperr.KindConflict, CodeVerificationTargetInUse)
		}
		if err != nil {
			return fmt.Errorf("authn: insert user: %w", err)
		}
		ip, agent := consentClient(in.ClientIP, in.UserAgent)
		for _, d := range in.Consents {
			err := q.InsertConsentRecord(ctx, authndb.InsertConsentRecordParams{
				ID: newID(), UserID: userID, DocumentCode: d.DocumentCode, Version: d.Version, Granted: d.Granted, RecordedAt: now,
				IpAddress: ip, UserAgent: agent,
			})
			if err != nil {
				return fmt.Errorf("authn: record consent: %w", err)
			}
		}
		deviceID, err := upsertDevice(ctx, q, userID, LoginInput{DeviceKey: in.DeviceKey, Platform: in.Platform}, now)
		if err != nil {
			return err
		}
		if pair, err = a.users.openSession(ctx, q, userID, deviceID, now); err != nil {
			return err
		}
		if err := insertAttempt(ctx, q, principalUser, a.users.targets.of(p.Email), outcomeSucceeded, now); err != nil {
			return err
		}
		return a.enqueueTx(ctx, tx, authMailArgs{Mail: mailWelcome, Ref: userID})
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("authn: complete registration: %w", err)
	}
	if outcome != nil {
		return TokenPair{}, outcome
	}
	return pair, nil
}

// consentClient is the client address and User-Agent a consent record
// keeps: the address as the edge resolved it, the User-Agent cut to
// maxUserAgentBytes on a character boundary; nil when absent.
func consentClient(addr netip.Addr, userAgent string) (ip, agent *string) {
	if addr.IsValid() {
		s := addr.Unmap().String()
		ip = &s
	}
	userAgent = strings.ToValidUTF8(strings.TrimSpace(userAgent), "")
	if len(userAgent) > maxUserAgentBytes {
		end := 0
		for i, r := range userAgent {
			if i+utf8.RuneLen(r) > maxUserAgentBytes {
				break
			}
			end = i + utf8.RuneLen(r)
		}
		userAgent = userAgent[:end]
	}
	if userAgent != "" {
		agent = &userAgent
	}
	return ip, agent
}

// checkConsents holds the decisions against the current agreements: each
// names a current version once, and every mandatory one is granted.
func (a *Accounts) checkConsents(ctx context.Context, decisions []ConsentDecision) error {
	current, err := a.users.reads.ListCurrentAgreements(ctx)
	if err != nil {
		return fmt.Errorf("authn: load agreements: %w", err)
	}
	type agreement struct{ code, version string }
	known := map[agreement]bool{}
	for _, c := range current {
		known[agreement{c.DocumentCode, c.Version}] = true
	}
	granted := map[agreement]bool{}
	seen := map[agreement]bool{}
	for i, d := range decisions {
		v := agreement{d.DocumentCode, d.Version}
		if !known[v] || seen[v] {
			return invalidField("/consents/" + strconv.Itoa(i))
		}
		seen[v] = true
		granted[v] = d.Granted
	}
	for _, c := range current {
		if c.Mandatory && !granted[agreement{c.DocumentCode, c.Version}] {
			return apperr.New(apperr.KindInvalid, CodeConsentRequired)
		}
	}
	return nil
}

// RequestPasswordReset queues a reset request. Like StartRegistration it
// does the same work for every well-formed address (USR-018);
// HandlePasswordResetRequest decides.
func (a *Accounts) RequestPasswordReset(ctx context.Context, email string) error {
	e, err := checkEmail(email)
	if err != nil {
		return err
	}
	return a.enqueue(ctx, resetRequestedArgs{Email: e})
}

// HandlePasswordResetRequest is the job of a reset request. An address
// without an active account gets nothing: a suspended account no more than
// an unknown one (ADR-0032 Karar 7), and the request did the same work for
// all of them. Otherwise the user's open reset is revoked and a new one
// written (USR-017), with its mail queued in the same transaction, unless
// the account's mail budget is spent (ADR-0032 Karar 6). An account
// without a password gets the mail too, as in Java: the reset sets one.
func (a *Accounts) HandlePasswordResetRequest(ctx context.Context, email string) error {
	cred, err := a.users.reads.GetUserCredentials(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("authn: look up the address: %w", err)
	}
	if cred.Status != UserActive {
		return nil
	}
	now := a.users.clock.Now()
	err = a.users.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		// The reset row is a child of the user: the user must stay active
		// until this commits (CLAUDE.md, ADR-0020).
		err := softdelete.LockActiveParent(ctx, tx, "users", cred.ID)
		if errors.Is(err, softdelete.ErrNotActive) {
			return nil // deleted since the look-up
		}
		if err != nil {
			return fmt.Errorf("authn: lock the user: %w", err)
		}
		q := authndb.New(tx)
		recent, err := q.CountRecentPasswordResets(ctx, authndb.CountRecentPasswordResetsParams{
			WindowStart: now.Add(-time.Hour), CooldownStart: now.Add(-LinkMailCooldown), UserID: cred.ID,
		})
		if err != nil {
			return fmt.Errorf("authn: count reset mails: %w", err)
		}
		if recent.InCooldown > 0 || recent.InWindow >= LinkMailsPerHour {
			a.users.metrics.mailHeld(heldAccount)
			a.users.logger.InfoContext(ctx, "password reset mail held back by the account's mail budget")
			return nil
		}
		if err := q.RevokeOpenPasswordResets(ctx, authndb.RevokeOpenPasswordResetsParams{Now: now, UserID: cred.ID}); err != nil {
			return fmt.Errorf("authn: revoke the open reset: %w", err)
		}
		id := newID()
		if err := q.InsertPasswordReset(ctx, authndb.InsertPasswordResetParams{ID: id, UserID: cred.ID, CreatedAt: now}); err != nil {
			return fmt.Errorf("authn: insert the reset request: %w", err)
		}
		return a.enqueueTx(ctx, tx, authMailArgs{Mail: mailResetLink, Ref: id})
	})
	if err != nil {
		return fmt.Errorf("authn: reset request: %w", err)
	}
	return nil
}

// PasswordResetCompletion is a reset: the mailed token, the new password
// and the device the request comes from.
type PasswordResetCompletion struct {
	Token     string
	Password  string
	DeviceKey string
	Platform  string
}

// CompletePasswordReset sets a new password with a reset token (USR-015).
// The token is consumed by one conditional statement in the transaction
// that writes the password; an unknown, expired, revoked and used token,
// and the token of an account that is no longer active, are the same
// error (USR-016). Every session of the user, the caller's included, ends,
// and no token pair is handed out: the user signs in again (USR-019).
//
// The reset proved the mailbox from the request's device, so that device
// becomes a known-good source and the sign-in target gets a SUCCEEDED
// attempt: a user locked out by someone else's guesses signs in with the
// new password at once (ADR-0032 Karar 7). The user is told by mail.
func (a *Accounts) CompletePasswordReset(ctx context.Context, in PasswordResetCompletion) error {
	if err := checkDeviceKey(in.DeviceKey); err != nil {
		return err
	}
	if err := CheckPassword(in.Password); err != nil {
		return err
	}
	invalid := apperr.New(apperr.KindInvalid, CodeResetLinkInvalid)
	if !wellFormedOpaque(in.Token) {
		return invalid
	}
	digest := hashOpaque(in.Token)
	now := a.users.clock.Now()
	_, err := a.users.reads.PeekPasswordReset(ctx, authndb.PeekPasswordResetParams{TokenDigest: digest, Now: now})
	if errors.Is(err, pgx.ErrNoRows) {
		return invalid
	}
	if err != nil {
		return fmt.Errorf("authn: look up the reset token: %w", err)
	}
	hash, err := a.users.passwords.Hash(ctx, in.Password)
	if err != nil {
		return fmt.Errorf("authn: hash password: %w", err)
	}
	var outcome error
	err = a.users.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		userID, err := q.ConsumePasswordReset(ctx, authndb.ConsumePasswordResetParams{Now: now, TokenDigest: digest})
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = invalid
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: consume the reset token: %w", err)
		}
		u, err := q.LockUserForSignIn(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && u.Status != UserActive) {
			// Deleted or suspended after the request: nothing to reset.
			// The error rolls the consumption back.
			return invalid
		}
		if err != nil {
			return fmt.Errorf("authn: lock user: %w", err)
		}
		if err := a.writePassword(ctx, tx, q, userID, hash, reasonReset, uuid.Nil, now); err != nil {
			return err
		}
		deviceID, err := upsertDevice(ctx, q, userID, LoginInput{DeviceKey: in.DeviceKey, Platform: in.Platform}, now)
		if err != nil {
			return err
		}
		if err := q.MarkDeviceProven(ctx, authndb.MarkDeviceProvenParams{Now: now, ID: deviceID}); err != nil {
			return fmt.Errorf("authn: mark the device: %w", err)
		}
		return insertAttempt(ctx, q, principalUser, a.users.targets.of(u.Email), outcomeSucceeded, now)
	})
	if err != nil {
		return fmt.Errorf("authn: complete password reset: %w", err)
	}
	return outcome
}

// PasswordChange is an in-app password change.
type PasswordChange struct {
	Current      string
	New          string
	Confirmation string
}

// ChangePassword sets a new password for the caller after checking the
// current one (USR-020). The caller's session stays, every other session
// ends, and the user's open reset token is revoked (USR-017). The checks
// that need no hash come first: the confirmation, the password rule
// (USR-021) and that the new password differs from the given current one.
//
// A wrong current password counts on the account's sign-in target like a
// wrong password at sign-in, and while the target is locked the change is
// refused unexamined (ADR-0032 Karar 7): a stolen session cannot guess the
// password here without limit. The known-device exemption of sign-in does
// not apply, since the caller's own device is the one that holds the
// session.
func (a *Accounts) ChangePassword(ctx context.Context, p UserPrincipal, in PasswordChange) error {
	if in.New != in.Confirmation {
		return apperr.New(apperr.KindInvalid, CodeConfirmationMismatch)
	}
	if err := CheckPassword(in.New); err != nil {
		return err
	}
	if in.New == in.Current {
		return apperr.New(apperr.KindInvalid, CodeNewEqualsCurrent)
	}
	mismatch := apperr.New(apperr.KindForbidden, CodeCurrentMismatch)
	gone := apperr.New(apperr.KindUnauthenticated, CodeSessionInactive)
	cur, err := a.users.reads.GetUserPassword(ctx, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return gone
	}
	if err != nil {
		return fmt.Errorf("authn: load password: %w", err)
	}
	now := a.users.clock.Now()
	target := a.users.targets.of(cur.Email)
	failures, locked, err := a.users.lockState(ctx, target, now)
	if err != nil {
		return err
	}
	ok, err := a.verifyCurrent(ctx, cur.PasswordHash, in.Current, locked)
	if err != nil {
		return err
	}
	if locked {
		if err := insertAttempt(ctx, a.users.reads, principalUser, target, outcomeLocked, now); err != nil {
			return err
		}
		return mismatch
	}
	if !ok {
		return a.users.fail(ctx, target, failures, now, mismatch)
	}
	hash, err := a.users.passwords.Hash(ctx, in.New)
	if err != nil {
		return fmt.Errorf("authn: hash password: %w", err)
	}
	var outcome error
	err = a.users.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		u, err := q.LockUserForSignIn(ctx, p.UserID)
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = gone
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: lock user: %w", err)
		}
		if u.PasswordHash == nil || cur.PasswordHash == nil || *u.PasswordHash != *cur.PasswordHash {
			// Changed since it was verified above: the current password
			// checked is not the account's password any more.
			outcome = mismatch
			return nil
		}
		if err := a.writePassword(ctx, tx, q, p.UserID, hash, reasonChanged, p.SessionID, now); err != nil {
			return err
		}
		return insertAttempt(ctx, q, principalUser, target, outcomeSucceeded, now)
	})
	if err != nil {
		return fmt.Errorf("authn: change password: %w", err)
	}
	return outcome
}

// verifyCurrent checks a current password against the stored hash. Every
// branch costs one argon2id verification: an account without a password
// is checked against the dummy, and a locked target against a decoy whose
// result is discarded.
func (a *Accounts) verifyCurrent(ctx context.Context, stored *string, current string, locked bool) (bool, error) {
	var ok bool
	var err error
	switch {
	case stored == nil:
		err = a.users.passwords.VerifyDummy(ctx, current)
	case locked:
		err = a.users.passwords.VerifyDecoy(ctx, *stored, current)
	default:
		ok, _, err = a.users.passwords.Verify(ctx, *stored, current)
	}
	if err != nil {
		return false, fmt.Errorf("authn: verify password: %w", err)
	}
	return ok, nil
}

// writePassword stores a new password hash under the user's row lock and
// moves the credential epoch, ends every session but keep (uuid.Nil keeps
// none), revokes the open reset token and queues the notice mail. The
// realtime connections of the ended sessions close after commit.
func (a *Accounts) writePassword(ctx context.Context, tx db.Querier, q *authndb.Queries, userID uuid.UUID, hash, reason string,
	keep uuid.UUID, now time.Time,
) error {
	if err := q.SetUserPassword(ctx, authndb.SetUserPasswordParams{PasswordHash: hash, Now: now, ID: userID}); err != nil {
		return fmt.Errorf("authn: store password: %w", err)
	}
	ended, err := q.RevokeUserSessionsForPassword(ctx, authndb.RevokeUserSessionsForPasswordParams{
		Now: now, Reason: reason, UserID: userID, KeepID: keep,
	})
	if err != nil {
		return fmt.Errorf("authn: end sessions: %w", err)
	}
	if err := q.RevokeOpenPasswordResets(ctx, authndb.RevokeOpenPasswordResetsParams{Now: now, UserID: userID}); err != nil {
		return fmt.Errorf("authn: revoke the open reset: %w", err)
	}
	tx.AfterCommit(func(ctx context.Context) {
		for _, sid := range ended {
			a.users.onRevoke(ctx, userID, sid)
		}
	})
	return a.enqueueTx(ctx, tx, authMailArgs{Mail: mailChanged, Ref: userID})
}

// Purge hard-deletes pending registrations and reset requests older than
// AccountRowRetention (M2). The authentication purge job calls it.
func (a *Accounts) Purge(ctx context.Context) (pending, resets int64, err error) {
	before := a.users.clock.Now().Add(-AccountRowRetention)
	if pending, err = a.users.reads.PurgePendingRegistrations(ctx, before); err != nil {
		return 0, 0, fmt.Errorf("authn: purge pending registrations: %w", err)
	}
	if resets, err = a.users.reads.PurgePasswordResets(ctx, before); err != nil {
		return pending, 0, fmt.Errorf("authn: purge reset requests: %w", err)
	}
	return pending, resets, nil
}

// enqueue inserts a job in a transaction of its own.
func (a *Accounts) enqueue(ctx context.Context, args river.JobArgs) error {
	err := a.users.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		return a.enqueueTx(ctx, tx, args)
	})
	if err != nil {
		return fmt.Errorf("authn: queue %s: %w", args.Kind(), err)
	}
	return nil
}

// enqueueTx inserts a job in tx.
func (a *Accounts) enqueueTx(ctx context.Context, tx db.Querier, args river.JobArgs) error {
	if _, err := a.queue.InsertTx(ctx, tx.PgxTx(), args, nil); err != nil {
		return fmt.Errorf("authn: queue %s: %w", args.Kind(), err)
	}
	return nil
}

// checkEmail normalizes an address (users.email's form) and accepts a
// bare RFC 5322 address of 3 to 254 characters.
func checkEmail(raw string) (string, error) {
	email := normalizeEmail(raw)
	addr, err := mail.ParseAddress(email)
	if n := utf8.RuneCountInString(email); err != nil || addr.Address != email || n < 3 || n > 254 {
		return "", invalidField("/email")
	}
	return email, nil
}

// invalidField is a REQUEST_INVALID naming one body member whose value the
// schema allows but the rule does not.
func invalidField(pointer string) error {
	return apperr.New(apperr.KindInvalid, httpx.CodeRequestInvalid).
		WithFields(apperr.FieldError{Pointer: pointer, Code: httpx.CodeFieldValueInvalid})
}

// emailTaken reports whether err is the unique violation of an active
// user's address (users_email_key).
func emailTaken(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_email_key"
}
