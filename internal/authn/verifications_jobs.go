package authn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/is-koala/koala-api/internal/authn/authndb"
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
// longer holds and the job could not find otherwise.
type contactChangedArgs struct {
	User     uuid.UUID `json:"user"`
	Channel  string    `json:"channel"`
	Previous string    `json:"previous,omitempty"`
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
	return w.verifications.SendContactNotice(ctx, job.Args.User, job.Args.Channel, job.Args.Previous)
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

// SendCode is the job of one code. A text message first takes its place
// under the global hourly cap; over it nothing is sent and the hold is
// counted (ADR-0033). The code is minted next, in a statement of its own
// that stores only its digest and commits before the provider is called;
// the code lives only in this call and in the message. A verification
// that ended or expired before its code sends nothing. An undeliverable
// recipient cancels the job instead of retrying it.
func (v *Verifications) SendCode(ctx context.Context, ref uuid.UUID, channel string) error {
	now := v.users.clock.Now()
	if channel == ChannelPhone {
		sent, err := v.users.reads.CountRecentTextMessages(ctx, now.Add(-time.Hour))
		if err != nil {
			return fmt.Errorf("authn: count text messages: %w", err)
		}
		if sent >= v.textsPerHour {
			v.users.metrics.verificationCode(ChannelPhone, codeHeld)
			v.users.logger.WarnContext(ctx, "verification text held back by the global hourly cap")
			return nil
		}
	}
	code, err := newVerificationCode()
	if err != nil {
		return err
	}
	row, err := v.users.reads.MintVerificationCode(ctx, authndb.MintVerificationCodeParams{
		CodeDigest: v.codeDigest(ref, code), Now: now, ID: ref,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("authn: mint the code: %w", err)
	}
	switch row.Channel {
	case ChannelEmail:
		msg, err := v.mails.Render(mailVerificationCode, row.Target, codeMailData{Code: code})
		if err != nil {
			return fmt.Errorf("authn: render the code mail: %w", err)
		}
		err = v.mail.Send(ctx, msg)
		if errors.Is(err, mailer.ErrUndeliverable) {
			v.users.metrics.verificationCode(row.Channel, codeUndeliverable)
			return fmt.Errorf("authn: code mail is undeliverable: %w", river.JobCancel(err))
		}
		if err != nil {
			return fmt.Errorf("authn: send the code mail: %w", err)
		}
	case ChannelPhone:
		err := v.texts.Send(ctx, sms.Message{To: row.Target, Text: fmt.Sprintf(textCode, code)})
		if errors.Is(err, sms.ErrUndeliverable) {
			v.users.metrics.verificationCode(row.Channel, codeUndeliverable)
			return fmt.Errorf("authn: code text is undeliverable: %w", river.JobCancel(err))
		}
		if err != nil {
			return fmt.Errorf("authn: send the code text: %w", err)
		}
	default:
		return fmt.Errorf("authn: unknown verification channel %q", row.Channel)
	}
	v.users.metrics.verificationCode(row.Channel, codeSent)
	return nil
}

// SendContactNotice tells the account holder that a confirmed
// verification changed the account's address or number: an address
// change at the previous address, a number change at the account's
// address. A user deleted since gets nothing.
func (v *Verifications) SendContactNotice(ctx context.Context, userID uuid.UUID, channel, previous string) error {
	who, err := v.users.reads.GetUserMailIdentity(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("authn: load the recipient: %w", err)
	}
	to, data := who.Email, noticeMailData{FirstName: who.FirstName, Changed: "telefon numarası"}
	if channel == ChannelEmail {
		data.Changed = "e-posta adresi"
		if previous != "" {
			to = previous
		}
	}
	msg, err := v.mails.Render(mailContactChanged, to, data)
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
