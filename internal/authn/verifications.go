package authn

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"

	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/mailer"
	"github.com/is-koala/koala-api/internal/platform/sms"
)

// The verification mails.
const (
	mailVerificationCode = "verification_code"
	mailContactChanged   = "contact_changed"
)

// textCode is the text message of a code (Java's
// verification.phone.code.sms, with a warning): one segment even in the
// Turkish alphabet's encoding.
const textCode = "Koala doğrulama kodunuz: %s. Bu kodu kimseyle paylaşmayın."

// codeKeyInfo is the HKDF context of the code digest key: the login
// pepper keys sign-in targets directly and the code digest only through
// this derivation, so neither use can stand in for the other.
const codeKeyInfo = "koala-api verification code v1"

// textSender delivers one text message; internal/integrations/iletimerkezi
// is the real one. It is called from River jobs only, outside any
// transaction.
type textSender interface {
	Send(ctx context.Context, m sms.Message) error
}

// Verifications runs the e-mail and phone verification of a signed-in
// user (ADR-0029 Karar 3, ADR-0033). The request writes the verification
// and queues its code job in one transaction; the job mints the code,
// stores only its keyed digest, commits, and then sends it. The code is
// never in the request, the job's arguments or the database.
type Verifications struct {
	users *Service
	queue jobQueue
	mail  mailSender
	texts textSender
	mails *mailer.Templates
	// codeKey keys the stored digest of a code.
	codeKey []byte
	// textsPerHour is the global hourly cap of codes by text message.
	textsPerHour int64
}

// NewVerifications returns the verification flows on users' database,
// clock and pepper. textsPerHour is the global hourly cap of text
// messages, 0 for DefaultTextsPerHour.
func NewVerifications(users *Service, queue jobQueue, mail mailSender, texts textSender, textsPerHour int) (*Verifications, error) {
	if users == nil || queue == nil || mail == nil || texts == nil {
		return nil, errors.New("authn: verifications need the user service, a job queue, a mail sender and a text sender")
	}
	key, err := hkdf.Key(sha256.New, users.targets.pepper, nil, codeKeyInfo, sha256.Size)
	if err != nil {
		return nil, fmt.Errorf("authn: derive the code key: %w", err)
	}
	mails, err := parseMailTemplates(mailVerificationCode, mailContactChanged)
	if err != nil {
		return nil, err
	}
	limit := int64(DefaultTextsPerHour)
	if textsPerHour > 0 {
		limit = int64(textsPerHour)
	}
	return &Verifications{users: users, queue: queue, mail: mail, texts: texts, mails: mails, codeKey: key, textsPerHour: limit}, nil
}

// resendError is VERIFICATION_RESEND_TOO_SOON with the wait the answer's
// Retry-After carries.
type resendError struct {
	err  *apperr.Error
	wait time.Duration
}

func (e *resendError) Error() string { return e.err.Error() }

func (e *resendError) Unwrap() error { return e.err }

// RetryAfter returns the wait an error asks the client to keep before it
// tries again, if it carries one.
func RetryAfter(err error) (time.Duration, bool) {
	var r *resendError
	if errors.As(err, &r) {
		return r.wait, true
	}
	return 0, false
}

// verificationTarget checks a target for its channel and returns the
// stored form and the budget key: an address normalized like users.email
// and its mailbox (sub-address and Gmail dots dropped, ADR-0032 Karar 6);
// a number in E.164, which is also its own key, and only one that may get
// a text message (TextableNumber).
func verificationTarget(channel, raw string) (target, budget string, err error) {
	switch channel {
	case ChannelEmail:
		email, err := checkTarget(raw)
		if err != nil {
			return "", "", err
		}
		return email, mailboxKey(email), nil
	case ChannelPhone:
		n, ok := NormalizePhone(raw)
		if !ok || !TextableNumber(n) {
			return "", "", invalidField("/target")
		}
		return n, n, nil
	}
	return "", "", invalidField("/channel")
}

// checkTarget is checkEmail for the verification's member.
func checkTarget(raw string) (string, error) {
	email, err := checkEmail(raw)
	if err != nil {
		return "", invalidField("/target")
	}
	return email, nil
}

// TargetKey is the keyed hash that stands for a target in the rate limit
// keys of a start: the budget key's, so every spelling of one mailbox or
// number is one target. A target that does not pass the checks is keyed
// as written; the request fails anyway.
func (v *Verifications) TargetKey(channel, raw string) []byte {
	if _, budget, err := verificationTarget(channel, raw); err == nil {
		return v.users.targets.of(budget)
	}
	return v.users.targets.of(strings.ToLower(strings.TrimSpace(raw)))
}

// targetLock is the advisory lock key of a keyed target: its first eight
// bytes, big-endian.
func targetLock(key []byte) int64 {
	var k int64
	for _, b := range key[:8] {
		k = k<<8 | int64(b)
	}
	return k
}

// Start opens a verification of a new address or number for the caller
// and queues its code (USR-008). The caller's open verification of the
// same channel and target is canceled (USR-010). The resend cooldown,
// the target's hourly budget (over every user) and the caller's daily cap
// per channel answer VERIFICATION_RESEND_TOO_SOON with the wait.
//
// The answer does not say whether the target is another account's: that
// is told only on confirmation, to the caller who entered the code sent
// to it (ADR-0033).
func (v *Verifications) Start(ctx context.Context, p UserPrincipal, channel, raw string) (Verification, error) {
	target, budget, err := verificationTarget(channel, raw)
	if err != nil {
		return Verification{}, err
	}
	key := v.users.targets.of(budget)
	now := v.users.clock.Now()
	id := newID()
	var outcome error
	err = v.users.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		// The verification is a child of the user: the user's row lock
		// keeps it active until this commits (CLAUDE.md, ADR-0020), and
		// serializes the user's starts for the daily cap.
		u, err := q.LockUserForSignIn(ctx, p.UserID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && u.Status != UserActive) {
			outcome = apperr.New(apperr.KindUnauthenticated, CodeSessionInactive)
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: lock user: %w", err)
		}
		// Every start of the target, from any user, counts and then
		// writes under this lock.
		if err := q.LockVerificationTarget(ctx, targetLock(key)); err != nil {
			return fmt.Errorf("authn: lock the target: %w", err)
		}
		wait, err := v.budgetWait(ctx, q, p.UserID, channel, key, now)
		if err != nil {
			return err
		}
		if wait > 0 {
			v.users.metrics.verificationCode(channel, codeThrottled)
			outcome = &resendError{err: apperr.New(apperr.KindRateLimited, CodeVerificationResendTooSoon), wait: wait}
			return nil
		}
		err = q.CancelOpenVerifications(ctx, authndb.CancelOpenVerificationsParams{
			Now: now, UserID: p.UserID, Channel: channel, Target: target,
		})
		if err != nil {
			return fmt.Errorf("authn: cancel the open verification: %w", err)
		}
		err = q.InsertVerification(ctx, authndb.InsertVerificationParams{
			ID: id, UserID: p.UserID, Channel: channel, Target: target, TargetKey: key,
			CreatedAt: now, ExpiresAt: now.Add(VerificationTTL),
		})
		if err != nil {
			return fmt.Errorf("authn: insert the verification: %w", err)
		}
		return v.enqueueTx(ctx, tx, verificationCodeArgs{Ref: id, Channel: channel})
	})
	if err != nil {
		return Verification{}, fmt.Errorf("authn: start verification: %w", err)
	}
	if outcome != nil {
		return Verification{}, outcome
	}
	return Verification{
		ID: id, Channel: channel, Status: StatusPending, MaskedTarget: maskTarget(channel, target),
		RemainingAttempts: VerificationAttempts, CreatedAt: now, ExpiresAt: now.Add(VerificationTTL),
	}, nil
}

// budgetWait is how long a start must wait under the resend budget, 0
// when it may go: the cooldown since the target's last code, the target's
// hourly budget and the user's daily cap of the channel.
func (v *Verifications) budgetWait(ctx context.Context, q *authndb.Queries, userID uuid.UUID, channel string, key []byte,
	now time.Time,
) (time.Duration, error) {
	var wait time.Duration
	target, err := q.TargetSendsSince(ctx, authndb.TargetSendsSinceParams{Since: now.Add(-time.Hour), TargetKey: key})
	if err != nil {
		return 0, fmt.Errorf("authn: count the target's codes: %w", err)
	}
	if target.Sends > 0 {
		wait = max(wait, target.LastAt.Add(VerificationCooldown).Sub(now))
	}
	if target.Sends >= VerificationsPerTargetHour {
		wait = max(wait, target.FirstAt.Add(time.Hour).Sub(now))
	}
	daily := int64(MailCodesPerUserDay)
	if channel == ChannelPhone {
		daily = TextsPerUserDay
	}
	user, err := q.UserSendsSince(ctx, authndb.UserSendsSinceParams{Since: now.Add(-24 * time.Hour), UserID: userID, Channel: channel})
	if err != nil {
		return 0, fmt.Errorf("authn: count the user's codes: %w", err)
	}
	if user.Sends >= daily {
		wait = max(wait, user.FirstAt.Add(24*time.Hour).Sub(now))
	}
	return wait, nil
}

// Confirm checks a code against one of the caller's verifications and,
// when it is right, writes the new address or number as verified
// (USR-014). The checks run in ADR-0029 Karar 3's order: the record and
// its owner (another user's is the 404 of a missing one, USR-011), its
// state (a confirmed one is a conflict whatever the code, USR-013; a
// burned one is exhausted, USR-009), its end before the code (USR-012),
// and the code. A wrong code is counted under the row lock and the
// fifth burns the verification, so five holds under concurrency.
//
// A target another active account holds is VERIFICATION_TARGET_IN_USE:
// the transaction rolls back and the verification is not consumed. Only
// the caller who entered the right code, the target's owner, learns it.
// The change is told by mail to the address on file before it; an
// address change also revokes the open password reset, mailed to the old
// address.
func (v *Verifications) Confirm(ctx context.Context, p UserPrincipal, id uuid.UUID, code string) (Verification, error) {
	now := v.users.clock.Now()
	notFound := apperr.New(apperr.KindNotFound, CodeVerificationNotFound)
	var out Verification
	var outcome error
	err := v.users.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		// The user's row first, then the verification's: Start takes them
		// in the same order.
		u, err := q.LockUserForSignIn(ctx, p.UserID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && u.Status != UserActive) {
			outcome = apperr.New(apperr.KindUnauthenticated, CodeSessionInactive)
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: lock user: %w", err)
		}
		row, err := q.LockVerification(ctx, authndb.LockVerificationParams{ID: id, UserID: p.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = notFound
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: lock the verification: %w", err)
		}
		switch {
		case row.ConfirmedAt != nil:
			outcome = apperr.New(apperr.KindConflict, CodeVerificationConfirmed)
			return nil
		case row.BurnedAt != nil:
			outcome = apperr.New(apperr.KindConflict, CodeVerificationExhausted)
			return nil
		case row.CanceledAt != nil:
			outcome = notFound
			return nil
		case !now.Before(row.ExpiresAt):
			outcome = apperr.New(apperr.KindInvalid, CodeVerificationExpired)
			return nil
		case !v.codeMatches(row.ID, row.CodeDigest, code):
			_, err := q.RecordFailedVerification(ctx, authndb.RecordFailedVerificationParams{
				MaxAttempts: VerificationAttempts, Now: now, ID: row.ID,
			})
			if err != nil {
				return fmt.Errorf("authn: count the wrong code: %w", err)
			}
			outcome = apperr.New(apperr.KindInvalid, CodeVerificationWrong)
			return nil
		}
		if err := q.ConfirmVerification(ctx, authndb.ConfirmVerificationParams{Now: now, ID: row.ID}); err != nil {
			return fmt.Errorf("authn: confirm the verification: %w", err)
		}
		if err := v.apply(ctx, tx, q, u, row.Channel, row.Target, now); err != nil {
			return err
		}
		confirmed := now
		out = Verification{
			ID: row.ID, Channel: row.Channel, Status: StatusConfirmed, MaskedTarget: maskTarget(row.Channel, row.Target),
			RemainingAttempts: VerificationAttempts - row.FailedAttempts, CreatedAt: row.CreatedAt,
			ExpiresAt: row.ExpiresAt, ConfirmedAt: &confirmed,
		}
		return nil
	})
	if err != nil {
		return Verification{}, fmt.Errorf("authn: confirm verification: %w", err)
	}
	if outcome != nil {
		return Verification{}, outcome
	}
	return out, nil
}

// apply writes a confirmed target on the locked user and queues the
// notice. A target another active account holds is a conflict, and the
// error rolls the confirmation back with the rest.
func (v *Verifications) apply(ctx context.Context, tx db.Querier, q *authndb.Queries, u authndb.LockUserForSignInRow,
	channel, target string, now time.Time,
) error {
	inUse := apperr.New(apperr.KindConflict, CodeVerificationTargetInUse)
	notice := contactChangedArgs{User: u.ID, Channel: channel}
	switch channel {
	case ChannelEmail:
		err := q.SetUserEmail(ctx, authndb.SetUserEmailParams{Email: target, ID: u.ID})
		if emailTaken(err) {
			return inUse
		}
		if err != nil {
			return fmt.Errorf("authn: write the address: %w", err)
		}
		if u.Email == target {
			return nil // the address on file, now verified: nothing changed
		}
		// The open reset link went to the old address; it does not
		// outlive the change.
		if err := q.RevokeOpenPasswordResets(ctx, authndb.RevokeOpenPasswordResetsParams{Now: now, UserID: u.ID}); err != nil {
			return fmt.Errorf("authn: revoke the open reset: %w", err)
		}
		notice.Previous = u.Email
	case ChannelPhone:
		err := q.SetUserPhone(ctx, authndb.SetUserPhoneParams{Phone: target, ID: u.ID})
		if phoneTaken(err) {
			return inUse
		}
		if err != nil {
			return fmt.Errorf("authn: write the number: %w", err)
		}
	default:
		return fmt.Errorf("authn: unknown verification channel %q", channel)
	}
	return v.enqueueTx(ctx, tx, notice)
}

// phoneTaken reports whether err is the unique violation of an active
// user's number (users_phone_key).
func phoneTaken(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_phone_key"
}

// Get returns one of the caller's verifications. Another user's, a
// burned and a canceled one are the 404 of a missing one (USR-011).
func (v *Verifications) Get(ctx context.Context, p UserPrincipal, id uuid.UUID) (Verification, error) {
	row, err := v.users.reads.GetVerification(ctx, authndb.GetVerificationParams{ID: id, UserID: p.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Verification{}, apperr.New(apperr.KindNotFound, CodeVerificationNotFound)
	}
	if err != nil {
		return Verification{}, fmt.Errorf("authn: load the verification: %w", err)
	}
	return verificationOf(row.ID, row.Channel, row.Target, row.CreatedAt, row.ExpiresAt, row.FailedAttempts,
		row.ConfirmedAt, v.users.clock.Now()), nil
}

// Cancel ends one of the caller's pending verifications; its code stops
// working. A confirmed one stays as it is. Another user's, a burned, an
// already canceled and a missing one are the same 404.
func (v *Verifications) Cancel(ctx context.Context, p UserPrincipal, id uuid.UUID) error {
	n, err := v.users.reads.CancelVerification(ctx, authndb.CancelVerificationParams{
		Now: v.users.clock.Now(), ID: id, UserID: p.UserID,
	})
	if err != nil {
		return fmt.Errorf("authn: cancel the verification: %w", err)
	}
	if n == 1 {
		return nil
	}
	_, err = v.users.reads.GetVerification(ctx, authndb.GetVerificationParams{ID: id, UserID: p.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.New(apperr.KindNotFound, CodeVerificationNotFound)
	}
	if err != nil {
		return fmt.Errorf("authn: load the verification: %w", err)
	}
	return nil
}

// VerificationPage is one page of the caller's open verifications and
// the cursors around it.
type VerificationPage struct {
	Items      []Verification
	NextCursor *string
	PrevCursor *string
}

// The keyset cursor of the list: its direction and the (created_at, id)
// position, base64url under a version prefix. It is opaque to the client
// and carries no filter, because the list has none.
const (
	cursorPrefix = "vrf1"
	cursorOlder  = "n"
	cursorNewer  = "p"
)

type listCursor struct {
	newer bool
	at    time.Time
	id    uuid.UUID
}

func (c listCursor) encode() *string {
	dir := cursorOlder
	if c.newer {
		dir = cursorNewer
	}
	raw := strings.Join([]string{cursorPrefix, dir, strconv.FormatInt(c.at.UnixNano(), 10), c.id.String()}, "|")
	s := base64.RawURLEncoding.EncodeToString([]byte(raw))
	return &s
}

func decodeCursor(s string) (listCursor, error) {
	invalid := apperr.New(apperr.KindInvalid, CodeCursorInvalid)
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return listCursor{}, invalid
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 || parts[0] != cursorPrefix || (parts[1] != cursorOlder && parts[1] != cursorNewer) {
		return listCursor{}, invalid
	}
	nanos, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return listCursor{}, invalid
	}
	id, err := uuid.Parse(parts[3])
	if err != nil {
		return listCursor{}, invalid
	}
	return listCursor{newer: parts[1] == cursorNewer, at: time.Unix(0, nanos).UTC(), id: id}, nil
}

// firstPage is the keyset position before every row: later than any
// timestamp and the greatest id.
func firstPage() listCursor {
	return listCursor{at: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), id: uuid.Max}
}

// List returns a page of the caller's open verifications, newest first
// (neither confirmed, canceled nor burned, and not expired). A malformed
// cursor is CURSOR_INVALID.
func (v *Verifications) List(ctx context.Context, p UserPrincipal, limit int32, cursor string) (VerificationPage, error) {
	pos, first := firstPage(), true
	if cursor != "" {
		c, err := decodeCursor(cursor)
		if err != nil {
			return VerificationPage{}, err
		}
		pos, first = c, false
	}
	now := v.users.clock.Now()
	var items []Verification
	var more bool
	if pos.newer {
		rows, err := v.users.reads.ListOpenVerificationsNewer(ctx, authndb.ListOpenVerificationsNewerParams{
			UserID: p.UserID, Now: now, AfterAt: pos.at, AfterID: pos.id, MaxRows: limit + 1,
		})
		if err != nil {
			return VerificationPage{}, fmt.Errorf("authn: list verifications: %w", err)
		}
		more = len(rows) > int(limit)
		for _, r := range rows[:min(len(rows), int(limit))] {
			items = append(items, verificationOf(r.ID, r.Channel, r.Target, r.CreatedAt, r.ExpiresAt, r.FailedAttempts, r.ConfirmedAt, now))
		}
		slices.Reverse(items)
	} else {
		rows, err := v.users.reads.ListOpenVerifications(ctx, authndb.ListOpenVerificationsParams{
			UserID: p.UserID, Now: now, BeforeAt: pos.at, BeforeID: pos.id, MaxRows: limit + 1,
		})
		if err != nil {
			return VerificationPage{}, fmt.Errorf("authn: list verifications: %w", err)
		}
		more = len(rows) > int(limit)
		for _, r := range rows[:min(len(rows), int(limit))] {
			items = append(items, verificationOf(r.ID, r.Channel, r.Target, r.CreatedAt, r.ExpiresAt, r.FailedAttempts, r.ConfirmedAt, now))
		}
	}
	page := VerificationPage{Items: items}
	if len(items) == 0 {
		return page, nil
	}
	newest, oldest := items[0], items[len(items)-1]
	if (pos.newer && more) || (!pos.newer && !first) {
		page.PrevCursor = listCursor{newer: true, at: newest.CreatedAt, id: newest.ID}.encode()
	}
	if (!pos.newer && more) || pos.newer {
		page.NextCursor = listCursor{at: oldest.CreatedAt, id: oldest.ID}.encode()
	}
	return page, nil
}

// verificationOf is a stored verification as the caller sees it.
func verificationOf(id uuid.UUID, channel, target string, created, expires time.Time, failed int32,
	confirmed *time.Time, now time.Time,
) Verification {
	return Verification{
		ID: id, Channel: channel, Status: verificationStatus(confirmed, expires, now),
		MaskedTarget: maskTarget(channel, target), RemainingAttempts: VerificationAttempts - failed,
		CreatedAt: created, ExpiresAt: expires, ConfirmedAt: confirmed,
	}
}

// newVerificationCode is six decimal digits, uniform over 000000-999999:
// crypto/rand.Int draws below the bound without modulo bias (ADR-0029
// Karar 3).
func newVerificationCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("authn: draw a code: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// codeDigest is the stored form of a code: HMAC-SHA256 under the code key
// over the verification id and the code. A six-digit code's plain SHA-256
// is reversed by trying all million values; the key keeps a leaked table
// from yielding a code (ADR-0033).
func (v *Verifications) codeDigest(id uuid.UUID, code string) []byte {
	mac := hmac.New(sha256.New, v.codeKey)
	_, _ = mac.Write(id[:])        // a hash.Hash never fails to write
	_, _ = mac.Write([]byte(code)) // a hash.Hash never fails to write
	return mac.Sum(nil)
}

// codeMatches compares a code with the stored digest in constant time. A
// verification whose code was not sent yet has no digest and matches
// nothing.
func (v *Verifications) codeMatches(id uuid.UUID, digest []byte, code string) bool {
	return digest != nil && hmac.Equal(digest, v.codeDigest(id, code))
}

// Purge hard-deletes the verifications older than VerificationRetention
// (M2). The authentication purge job calls it.
func (v *Verifications) Purge(ctx context.Context) (int64, error) {
	n, err := v.users.reads.PurgeVerifications(ctx, v.users.clock.Now().Add(-VerificationRetention))
	if err != nil {
		return 0, fmt.Errorf("authn: purge verifications: %w", err)
	}
	return n, nil
}

// enqueueTx inserts a job in tx.
func (v *Verifications) enqueueTx(ctx context.Context, tx db.Querier, args river.JobArgs) error {
	if _, err := v.queue.InsertTx(ctx, tx.PgxTx(), args, nil); err != nil {
		return fmt.Errorf("authn: queue %s: %w", args.Kind(), err)
	}
	return nil
}
