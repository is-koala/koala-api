package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/mailer"
	"github.com/is-koala/koala-api/internal/platform/sms"
)

// Attempts of the verification jobs. A code is worth sending only within
// its ten minutes: River's backoff (attempt⁴ seconds) spends about six
// minutes on five attempts, and an attempt after the end sends nothing.
// Every attempt mints a new code, so a late one still sends a working
// code.
const (
	codeAttempts   = 5
	noticeAttempts = 10
)

// verificationCodeArgs is the job of one code: the verification and its
// channel. It carries neither the code nor the target.
type verificationCodeArgs struct {
	Ref     uuid.UUID `json:"ref"`
	Channel string    `json:"channel"`
}

// Kind is the job kind in River's tables.
func (verificationCodeArgs) Kind() string { return "verification_code" }

// InsertOpts are the job's River options.
func (verificationCodeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: codeAttempts}
}

// contactChangedArgs is the notice of a confirmed change: the user, the
// channel, and for an address the previous one, which the account no
// longer holds and the job could not find otherwise. Released is the
// notice of the account a number moved away from (ADR-0033 Karar 10).
type contactChangedArgs struct {
	User     uuid.UUID `json:"user"`
	Channel  string    `json:"channel"`
	Previous string    `json:"previous,omitempty"`
	Released bool      `json:"released,omitempty"`
}

// Kind is the job kind in River's tables.
func (contactChangedArgs) Kind() string { return "contact_changed" }

// InsertOpts are the job's River options.
func (contactChangedArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: noticeAttempts}
}

// AddWorkers registers the workers of the verification jobs on workers.
func (v *Verifications) AddWorkers(workers *river.Workers) error {
	errs := []error{
		river.AddWorkerSafely(workers, &codeWorker{verifications: v}),
		river.AddWorkerSafely(workers, &noticeWorker{verifications: v}),
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("authn: verification workers: %w", err)
	}
	return nil
}

type codeWorker struct {
	river.WorkerDefaults[verificationCodeArgs]

	verifications *Verifications
}

func (w *codeWorker) Work(ctx context.Context, job *river.Job[verificationCodeArgs]) error {
	return w.verifications.SendCode(ctx, job.Args.Ref, job.Args.Channel)
}

type noticeWorker struct {
	river.WorkerDefaults[contactChangedArgs]

	verifications *Verifications
}

func (w *noticeWorker) Work(ctx context.Context, job *river.Job[contactChangedArgs]) error {
	a := job.Args
	return w.verifications.SendContactNotice(ctx, a.User, a.Channel, a.Previous, a.Released)
}

// codeMailData is what the code mail reads: the code only. The address
// is not verified yet, so the mail carries nothing the account holder
// typed, not even the account's name (ADR-0032 Karar 3's rule).
type codeMailData struct {
	Code string
}

// noticeMailData is what the change notice reads.
type noticeMailData struct {
	FirstName string
	// Changed names what changed, in the mail's language.
	Changed string
}

// SendCode is the job of one code (ADR-0033 Karar 1 and 4).
//
//   - The code first takes its place under the channel's global hourly
//     cap. Over it the code is held back: the verification is marked held
//     (a code an earlier, failed attempt minted is dropped), which takes
//     it out of the resend budget, its first hold is counted, and the job
//     snoozes while enough of the ten minutes is left; otherwise it ends
//     without a code.
//   - The code is then minted in a transaction of its own that takes the
//     target's lock, locks the verification, and finds it open, its user
//     active and at least VerificationSendWindow of its life left. A held
//     verification spent no budget when it was held, so the resend budget
//     is checked again here, as a start checks it: when it no longer
//     allows the code, the verification ends unsent (canceled). Only the
//     code's digest is stored; the transaction commits before the provider
//     is called, and the code lives only in this call and in the message.
//     A verification that fails the other checks gets no code and is
//     marked held.
//   - An undeliverable recipient cancels the job instead of retrying it.
func (v *Verifications) SendCode(ctx context.Context, ref uuid.UUID, channel string) error {
	now := v.users.clock.Now()
	sent, err := v.users.reads.CountRecentCodes(ctx, authndb.CountRecentCodesParams{Channel: channel, Since: now.Add(-time.Hour)})
	if err != nil {
		return fmt.Errorf("authn: count recent codes: %w", err)
	}
	if limit, ok := v.perHour[channel]; ok && sent >= limit {
		return v.hold(ctx, ref, channel, now)
	}
	code, err := newVerificationCode()
	if err != nil {
		return err
	}
	key, err := v.users.reads.GetVerificationTargetKey(ctx, ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // purged
	}
	if err != nil {
		return fmt.Errorf("authn: load the verification's target: %w", err)
	}
	var target string
	var found, abandoned bool
	err = v.users.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		// The target's lock first, then the row (ADR-0033 Karar 5): the
		// budget is counted and spent as one step, as at a start.
		if err := q.LockVerificationTarget(ctx, targetLock(key)); err != nil {
			return fmt.Errorf("authn: lock the target: %w", err)
		}
		row, err := q.LockVerificationForCode(ctx, authndb.LockVerificationForCodeParams{
			ID: ref, SendBefore: now.Add(VerificationSendWindow),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: lock the verification: %w", err)
		}
		if row.Channel != channel {
			return fmt.Errorf("authn: the job of a %s code found a %s verification", channel, row.Channel)
		}
		if row.HeldAt != nil && row.CodeSentAt == nil {
			wait, err := v.budgetWait(ctx, q, row.UserID, channel, row.TargetKey, now)
			if err != nil {
				return err
			}
			if wait > 0 {
				if err := q.AbandonVerificationCode(ctx, authndb.AbandonVerificationCodeParams{Now: now, ID: ref}); err != nil {
					return fmt.Errorf("authn: end the held verification: %w", err)
				}
				abandoned = true
				return nil
			}
		}
		err = q.MintVerificationCode(ctx, authndb.MintVerificationCodeParams{CodeDigest: v.codeDigest(ref, code), Now: now, ID: ref})
		if err != nil {
			return fmt.Errorf("authn: mint the code: %w", err)
		}
		target, found = row.Target, true
		return nil
	})
	if err != nil {
		return fmt.Errorf("authn: code job: %w", err)
	}
	if abandoned {
		v.users.metrics.verificationCode(channel, codeThrottled)
		v.users.logger.InfoContext(ctx, "held verification code ended unsent: the resend budget no longer allows it",
			slog.String("channel", channel))
		return nil
	}
	if !found {
		// Ended, its user gone, or too late to be worth sending: no
		// code, and an open one spends no budget.
		if _, err := v.users.reads.HoldVerificationCode(ctx, authndb.HoldVerificationCodeParams{Now: now, ID: ref}); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("authn: mark the verification held: %w", err)
		}
		return nil
	}
	switch channel {
	case ChannelEmail:
		msg, err := v.mails.Render(mailVerificationCode, target, codeMailData{Code: code})
		if err != nil {
			return fmt.Errorf("authn: render the code mail: %w", err)
		}
		err = v.mail.Send(ctx, msg)
		if errors.Is(err, mailer.ErrUndeliverable) {
			v.users.metrics.verificationCode(channel, codeUndeliverable)
			return fmt.Errorf("authn: code mail is undeliverable: %w", river.JobCancel(err))
		}
		if err != nil {
			return fmt.Errorf("authn: send the code mail: %w", err)
		}
	case ChannelPhone:
		err := v.texts.Send(ctx, sms.Message{To: target, Text: fmt.Sprintf(textCode, code)})
		if errors.Is(err, sms.ErrUndeliverable) {
			v.users.metrics.verificationCode(channel, codeUndeliverable)
			return fmt.Errorf("authn: code text is undeliverable: %w", river.JobCancel(err))
		}
		if err != nil {
			return fmt.Errorf("authn: send the code text: %w", err)
		}
	default:
		return fmt.Errorf("authn: unknown verification channel %q", channel)
	}
	v.users.metrics.verificationCode(channel, codeSent)
	return nil
}

// hold holds a code back under its channel's global cap: the
// verification is marked held, its first hold counted (a snooze is not a
// new hold), and the job snoozes for
// CapSnooze while VerificationSendWindow of the verification's life is
// left after it. Otherwise the job ends; the verification stays open,
// without a code, until it expires.
func (v *Verifications) hold(ctx context.Context, ref uuid.UUID, channel string, now time.Time) error {
	held, err := v.users.reads.HoldVerificationCode(ctx, authndb.HoldVerificationCodeParams{Now: now, ID: ref})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // ended
	}
	if err != nil {
		return fmt.Errorf("authn: mark the verification held: %w", err)
	}
	if held.FirstHold {
		v.users.metrics.verificationCode(channel, codeHeld)
	}
	expires := held.ExpiresAt
	if expires.Sub(now) >= CapSnooze+VerificationSendWindow {
		v.users.logger.WarnContext(ctx, "verification code held back by the global hourly cap; snoozed",
			slog.String("channel", channel))
		return fmt.Errorf("authn: %s code held back by the global cap: %w", channel, river.JobSnooze(CapSnooze))
	}
	v.users.logger.WarnContext(ctx, "verification code held back by the global hourly cap; given up",
		slog.String("channel", channel))
	return nil
}

// SendContactNotice tells an account holder that a confirmed verification
// changed the account: an address change at the previous address, a
// number change at the account's address, and to the account a number
// moved away from, that it lost it (ADR-0033 Karar 10). A user deleted
// since gets nothing.
func (v *Verifications) SendContactNotice(ctx context.Context, userID uuid.UUID, channel, previous string, released bool) error {
	who, err := v.users.reads.GetUserMailIdentity(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("authn: load the recipient: %w", err)
	}
	mail, to, data := mailContactChanged, who.Email, noticeMailData{FirstName: who.FirstName, Changed: "telefon numarası"}
	switch {
	case released:
		mail = mailPhoneReleased
	case channel == ChannelEmail:
		data.Changed = "e-posta adresi"
		if previous != "" {
			to = previous
		}
	}
	msg, err := v.mails.Render(mail, to, data)
	if err != nil {
		return fmt.Errorf("authn: render the change notice: %w", err)
	}
	err = v.mail.Send(ctx, msg)
	if errors.Is(err, mailer.ErrUndeliverable) {
		return fmt.Errorf("authn: change notice is undeliverable: %w", river.JobCancel(err))
	}
	if err != nil {
		return fmt.Errorf("authn: send the change notice: %w", err)
	}
	return nil
}
