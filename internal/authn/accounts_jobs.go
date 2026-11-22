package authn

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/platform/mailer"
)

// Attempts of the account jobs. A link mail is retried for a few hours
// (River's backoff is attempt⁴ seconds); every attempt mints a new token,
// so a late one still sends a working link.
const (
	requestAttempts = 5
	mailAttempts    = 10
)

// registrationRequestedArgs is the job of a registration start. It carries
// the address and the names: deciding in the job is what keeps the request
// from telling a registered address apart (ADR-0032).
type registrationRequestedArgs struct {
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Employer  bool   `json:"employer"`
}

// Kind is the job kind in River's tables.
func (registrationRequestedArgs) Kind() string { return "registration_requested" }

// InsertOpts are the job's River options.
func (registrationRequestedArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: requestAttempts}
}

// resetRequestedArgs is the job of a password reset request.
type resetRequestedArgs struct {
	Email string `json:"email"`
}

// Kind is the job kind in River's tables.
func (resetRequestedArgs) Kind() string { return "password_reset_requested" }

// InsertOpts are the job's River options.
func (resetRequestedArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: requestAttempts}
}

// authMailArgs is the job of one account mail: which mail and the row it
// is about (the pending registration, the reset request or the user). It
// carries no token and no address.
type authMailArgs struct {
	Mail string    `json:"mail"`
	Ref  uuid.UUID `json:"ref"`
}

// Kind is the job kind in River's tables.
func (authMailArgs) Kind() string { return "auth_mail" }

// InsertOpts are the job's River options.
func (authMailArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{MaxAttempts: mailAttempts} }

// AddWorkers registers the workers of the account jobs on workers.
func (a *Accounts) AddWorkers(workers *river.Workers) error {
	errs := []error{
		river.AddWorkerSafely(workers, &registrationWorker{accounts: a}),
		river.AddWorkerSafely(workers, &resetWorker{accounts: a}),
		river.AddWorkerSafely(workers, &mailWorker{accounts: a}),
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("authn: account workers: %w", err)
	}
	return nil
}

type registrationWorker struct {
	river.WorkerDefaults[registrationRequestedArgs]

	accounts *Accounts
}

func (w *registrationWorker) Work(ctx context.Context, job *river.Job[registrationRequestedArgs]) error {
	return w.accounts.HandleRegistrationRequest(ctx, RegistrationRequest(job.Args))
}

type resetWorker struct {
	river.WorkerDefaults[resetRequestedArgs]

	accounts *Accounts
}

func (w *resetWorker) Work(ctx context.Context, job *river.Job[resetRequestedArgs]) error {
	return w.accounts.HandlePasswordResetRequest(ctx, job.Args.Email)
}

type mailWorker struct {
	river.WorkerDefaults[authMailArgs]

	accounts *Accounts
}

func (w *mailWorker) Work(ctx context.Context, job *river.Job[authMailArgs]) error {
	return w.accounts.SendAccountMail(ctx, job.Args.Mail, job.Args.Ref)
}

// mailData is what the account templates read.
type mailData struct {
	FirstName string
	Link      string
}

// SendAccountMail is the job of one account mail. A link mail mints its
// token first, in a statement of its own that commits before the relay is
// called (no transaction is open during the send); the token lives only
// in this call and in the mail. A row that ended before its mail (a
// consumed or revoked registration, a deleted user) sends nothing.
func (a *Accounts) SendAccountMail(ctx context.Context, kind string, ref uuid.UUID) error {
	msg, ok, err := a.prepareMail(ctx, kind, ref)
	if err != nil || !ok {
		return err
	}
	if err := a.sender.Send(ctx, msg); err != nil {
		return fmt.Errorf("authn: send %s mail: %w", kind, err)
	}
	return nil
}

func (a *Accounts) prepareMail(ctx context.Context, kind string, ref uuid.UUID) (mailer.Message, bool, error) {
	now := a.users.clock.Now()
	var to string
	data := mailData{}
	switch kind {
	case mailRegistrationLink:
		token := newOpaque()
		row, err := a.users.reads.MintRegistrationLink(ctx, authndb.MintRegistrationLinkParams{
			TokenHash: hashOpaque(token), ExpiresAt: now.Add(RegistrationLinkTTL), ID: ref,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return mailer.Message{}, false, nil
		}
		if err != nil {
			return mailer.Message{}, false, fmt.Errorf("authn: mint registration link: %w", err)
		}
		to, data.FirstName, data.Link = row.Email, row.FirstName, a.link(registrationLinkPath, token)
	case mailResetLink:
		token := newOpaque()
		userID, err := a.users.reads.MintPasswordReset(ctx, authndb.MintPasswordResetParams{
			TokenDigest: hashOpaque(token), ExpiresAt: now.Add(ResetLinkTTL), ID: ref,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return mailer.Message{}, false, nil
		}
		if err != nil {
			return mailer.Message{}, false, fmt.Errorf("authn: mint reset link: %w", err)
		}
		who, found, err := a.recipient(ctx, userID)
		if err != nil || !found {
			return mailer.Message{}, false, err
		}
		to, data.FirstName, data.Link = who.Email, who.FirstName, a.link(resetLinkPath, token)
	case mailChanged:
		who, found, err := a.recipient(ctx, ref)
		if err != nil || !found {
			return mailer.Message{}, false, err
		}
		to, data.FirstName = who.Email, who.FirstName
	default:
		return mailer.Message{}, false, fmt.Errorf("authn: unknown account mail %q", kind)
	}
	msg, err := a.mails.Render(kind, to, data)
	if err != nil {
		return mailer.Message{}, false, fmt.Errorf("authn: render %s mail: %w", kind, err)
	}
	return msg, true, nil
}

// recipient loads the user a mail goes to; found is false for a user
// deleted since.
func (a *Accounts) recipient(ctx context.Context, userID uuid.UUID) (authndb.GetUserMailIdentityRow, bool, error) {
	who, err := a.users.reads.GetUserMailIdentity(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return who, false, nil
	}
	if err != nil {
		return who, false, fmt.Errorf("authn: load the recipient: %w", err)
	}
	return who, true, nil
}

// link is the web address that opens path with token.
func (a *Accounts) link(path, token string) string {
	u := a.web.JoinPath(path)
	u.RawQuery = url.Values{"token": {token}}.Encode()
	return u.String()
}
