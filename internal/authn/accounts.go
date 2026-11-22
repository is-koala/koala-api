package authn

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/mail"
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
}

// NewAccounts returns the account flows on users' database and clock.
// webBaseURL is the web origin the mailed links open.
func NewAccounts(users *Service, queue jobQueue, sender mailSender, webBaseURL string) (*Accounts, error) {
	if users == nil || queue == nil || sender == nil {
		return nil, errors.New("authn: accounts need the user service, a job queue and a mail sender")
	}
	web, err := url.Parse(webBaseURL)
	if err != nil || (web.Scheme != "https" && web.Scheme != "http") || web.Host == "" {
		return nil, errors.New("authn: the web base URL must be an absolute http or https URL")
	}
	sub, err := fs.Sub(mailTemplates, "mail")
	if err != nil {
		return nil, fmt.Errorf("authn: mail templates: %w", err)
	}
	mails, err := mailer.ParseTemplates(sub)
	if err != nil {
		return nil, fmt.Errorf("authn: mail templates: %w", err)
	}
	if err := mails.Check(mailRegistrationLink, mailResetLink, mailChanged); err != nil {
		return nil, fmt.Errorf("authn: mail templates: %w", err)
	}
	return &Accounts{users: users, queue: queue, sender: sender, mails: mails, web: web}, nil
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
// transaction, unless the address's mail budget is spent (ADR-0032).
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
		recent, err := q.CountRecentRegistrationMails(ctx, authndb.CountRecentRegistrationMailsParams{
			WindowStart: now.Add(-time.Hour), CooldownStart: now.Add(-LinkMailCooldown), Email: in.Email,
		})
		if err != nil {
			return fmt.Errorf("authn: count registration mails: %w", err)
		}
		if recent.InCooldown > 0 || recent.InWindow >= LinkMailsPerHour {
			a.users.logger.InfoContext(ctx, "registration mail held back by the address's mail budget")
			return nil
		}
		err = q.RevokeOpenPendingRegistrations(ctx, authndb.RevokeOpenPendingRegistrationsParams{Now: now, Email: in.Email})
		if err != nil {
			return fmt.Errorf("authn: revoke the open registration: %w", err)
		}
		id := newID()
		err = q.InsertPendingRegistration(ctx, authndb.InsertPendingRegistrationParams{
			ID: id, Email: in.Email, FirstName: in.FirstName, LastName: in.LastName, Employer: in.Employer, CreatedAt: now,
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
// password, the consents and the device the session opens on.
type RegistrationCompletion struct {
	Token     string
	Password  string
	Consents  []ConsentDecision
	DeviceKey string
	Platform  string
}

// CompleteRegistration creates the user from the pending registration and
// signs it in on the request's device (USR-006). The consents are checked
// first, before the token is read: a missing mandatory consent neither
// consumes the token nor says whether it is valid (USR-005). The token is
// consumed by one conditional statement in the transaction that writes
// the user, the consents and the session, so it works once (USR-004).
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
		for _, d := range in.Consents {
			err := q.InsertConsentRecord(ctx, authndb.InsertConsentRecordParams{
				ID: newID(), UserID: userID, DocumentCode: d.DocumentCode, Version: d.Version, Granted: d.Granted, RecordedAt: now,
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
		return insertAttempt(ctx, q, principalUser, a.users.targets.of(p.Email), outcomeSucceeded, now)
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("authn: complete registration: %w", err)
	}
	if outcome != nil {
		return TokenPair{}, outcome
	}
	return pair, nil
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
// without an account gets nothing. Otherwise the user's open reset is
// revoked and a new one written (USR-017), with its mail queued in the same
// transaction, unless the account's mail budget is spent (ADR-0032).
func (a *Accounts) HandlePasswordResetRequest(ctx context.Context, email string) error {
	cred, err := a.users.reads.GetUserCredentials(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("authn: look up the address: %w", err)
	}
	now := a.users.clock.Now()
	err = a.users.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		recent, err := q.CountRecentPasswordResets(ctx, authndb.CountRecentPasswordResetsParams{
			WindowStart: now.Add(-time.Hour), CooldownStart: now.Add(-LinkMailCooldown), UserID: cred.ID,
		})
		if err != nil {
			return fmt.Errorf("authn: count reset mails: %w", err)
		}
		if recent.InCooldown > 0 || recent.InWindow >= LinkMailsPerHour {
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

// CompletePasswordReset sets a new password with a reset token (USR-015).
// The token is consumed by one conditional statement in the transaction
// that writes the password; an unknown, expired, revoked and used token
// are the same error (USR-016). Every session of the user, the caller's
// included, ends, and no token pair is handed out: the user signs in again
// (USR-019). The user is told by mail.
func (a *Accounts) CompletePasswordReset(ctx context.Context, token, newPassword string) error {
	if err := CheckPassword(newPassword); err != nil {
		return err
	}
	invalid := apperr.New(apperr.KindInvalid, CodeResetLinkInvalid)
	if !wellFormedOpaque(token) {
		return invalid
	}
	digest := hashOpaque(token)
	now := a.users.clock.Now()
	_, err := a.users.reads.PeekPasswordReset(ctx, authndb.PeekPasswordResetParams{TokenDigest: digest, Now: now})
	if errors.Is(err, pgx.ErrNoRows) {
		return invalid
	}
	if err != nil {
		return fmt.Errorf("authn: look up the reset token: %w", err)
	}
	hash, err := a.users.passwords.Hash(ctx, newPassword)
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
		_, err = q.LockUserForSignIn(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			// The user was deleted after the request: nothing to reset.
			// The error rolls the consumption back.
			return invalid
		}
		if err != nil {
			return fmt.Errorf("authn: lock user: %w", err)
		}
		return a.writePassword(ctx, tx, q, userID, hash, reasonReset, uuid.Nil, now)
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
	stored, err := a.users.reads.GetUserPasswordHash(ctx, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return gone
	}
	if err != nil {
		return fmt.Errorf("authn: load password: %w", err)
	}
	if stored == nil {
		if err := a.users.passwords.VerifyDummy(ctx, in.Current); err != nil {
			return fmt.Errorf("authn: verify password: %w", err)
		}
		return mismatch
	}
	ok, _, err := a.users.passwords.Verify(ctx, *stored, in.Current)
	if err != nil {
		return fmt.Errorf("authn: verify password: %w", err)
	}
	if !ok {
		return mismatch
	}
	hash, err := a.users.passwords.Hash(ctx, in.New)
	if err != nil {
		return fmt.Errorf("authn: hash password: %w", err)
	}
	now := a.users.clock.Now()
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
		if u.PasswordHash == nil || *u.PasswordHash != *stored {
			// Changed since it was verified above: the current password
			// checked is not the account's password any more.
			outcome = mismatch
			return nil
		}
		return a.writePassword(ctx, tx, q, p.UserID, hash, reasonChanged, p.SessionID, now)
	})
	if err != nil {
		return fmt.Errorf("authn: change password: %w", err)
	}
	return outcome
}

// writePassword stores a new password hash under the user's row lock,
// ends every session but keep (uuid.Nil keeps none), revokes the open
// reset token and queues the notice mail. The realtime connections of the
// ended sessions close after commit.
func (a *Accounts) writePassword(ctx context.Context, tx db.Querier, q *authndb.Queries, userID uuid.UUID, hash, reason string,
	keep uuid.UUID, now time.Time,
) error {
	if err := q.UpdateUserPasswordHash(ctx, authndb.UpdateUserPasswordHashParams{ID: userID, PasswordHash: &hash}); err != nil {
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
