package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/integrations/iletimerkezi"
	"github.com/is-koala/koala-api/internal/integrations/smtp"
	"github.com/is-koala/koala-api/internal/media"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/idempotency"
	"github.com/is-koala/koala-api/internal/platform/jobs"
	"github.com/is-koala/koala-api/internal/platform/mailer"
	"github.com/is-koala/koala-api/internal/platform/sms"
	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
	"github.com/is-koala/koala-api/internal/realtime"
)

// modules are the domain modules the router mounts and the workers run.
// authHTTP and admin.http together implement the generated strict server
// interface (strictServer).
type modules struct {
	users         *authn.Service
	staff         *authn.StaffService
	accounts      *authn.Accounts
	verifications *authn.Verifications
	authHTTP      *authn.HTTP
	admin         staffModule
	hub           *realtime.Hub
	realtime      *realtime.Handler
	purge         *authPurgeWorker
	// The Idempotency-Key store (ADR-0034) and its purge.
	idempotency      *idempotency.Store
	idempotencyPurge *idempotencyPurgeWorker
	// Media uploads (ADR-0035): the service runs the jobs, mediaHTTP
	// serves /v1/me/media.
	media     *media.Service
	mediaHTTP *media.HTTP
	// The profile and education operations (ADR-0036).
	usersHTTP *usersHandlers
}

// wireModules builds the modules on database, their metrics on reg.
// Authentication comes first: the realtime layer checks its sessions, and
// it closes a session's connections when authentication revokes the
// session (ADR-0022).
func wireModules(ctx context.Context, cfg config.Config, database *db.DB, clk clock.Clock, logger *slog.Logger,
	reg prometheus.Registerer,
) (modules, error) {
	var hub *realtime.Hub
	onRevoke := func(_ context.Context, userID, sessionID uuid.UUID) {
		if hub != nil {
			hub.DisconnectSession(realtime.UserID(userID.String()), sessionID.String())
		}
	}
	metrics, err := authn.NewMetrics(reg)
	if err != nil {
		return modules{}, fmt.Errorf("wire authn: %w", err)
	}
	pepper, err := loginPepper(ctx, cfg.LoginPepper, logger)
	if err != nil {
		return modules{}, err
	}
	users, staff, err := wireAuthn(ctx, cfg, database, clk, logger, metrics, pepper, onRevoke)
	if err != nil {
		return modules{}, err
	}
	// The store's keys are derived from the login pepper with their own
	// HKDF contexts (ADR-0034 Karar 3).
	idem, err := idempotency.NewStore(database, pepper, clk)
	if err != nil {
		return modules{}, fmt.Errorf("wire idempotency: %w", err)
	}
	accounts, verifications, err := wireAccounts(cfg, database, users, logger)
	if err != nil {
		return modules{}, err
	}
	sessions := realtimeSessions{users: users}
	hub, err = realtime.NewHub(noConversations{}, sessions, clk, logger, realtime.DefaultOptions())
	if err != nil {
		return modules{}, fmt.Errorf("wire realtime: %w", err)
	}
	ticketStore := realtime.NewPostgresTickets(database.Pool())
	tickets := realtime.NewTickets(ticketStore, sessions, clk, logger)
	opts := realtime.DefaultHandlerOptions()
	opts.AllowedOrigins = splitList(cfg.RealtimeOrigins)
	handler, err := realtime.NewHandler(hub, tickets, realtimeAuth{users: users}, logger, opts)
	if err != nil {
		return modules{}, fmt.Errorf("wire realtime: %w", err)
	}
	admin, err := wireStaff(database, staff)
	if err != nil {
		return modules{}, err
	}
	mediaService, mediaHTTP, err := wireMedia(ctx, cfg, database, clk, logger, reg)
	if err != nil {
		return modules{}, err
	}
	usersHTTP, err := wireUsers(database, verifications, mediaService, clk)
	if err != nil {
		return modules{}, err
	}
	return modules{
		users: users, staff: staff, accounts: accounts, verifications: verifications,
		authHTTP: authn.NewHTTP(users, staff, accounts, verifications),
		admin:    admin, hub: hub, realtime: handler,
		purge: &authPurgeWorker{
			users: users, staff: staff, accounts: accounts, verifications: verifications,
			tickets: ticketStore, clock: clk, logger: logger,
		},
		idempotency:      idem,
		idempotencyPurge: &idempotencyPurgeWorker{store: idem, logger: logger},
		media:            mediaService,
		mediaHTTP:        mediaHTTP,
		usersHTTP:        usersHTTP,
	}, nil
}

// wireAuthn builds the two authentication services (ADR-0025). Each token
// class signs with its own ring. Outside staging and production a ring
// that is not configured is generated per process (config.ServeAuthErrors
// refuses that where it matters).
func wireAuthn(ctx context.Context, cfg config.Config, database *db.DB, clk clock.Clock, logger *slog.Logger,
	metrics *authn.Metrics, pepper []byte, onRevoke authn.RevokeHook,
) (*authn.Service, *authn.StaffService, error) {
	// One argon2id hash per CPU at a time: each holds 19 MiB for tens of
	// milliseconds; more in parallel would only queue on the CPUs.
	passwords, err := authn.NewPasswords(authn.DefaultParams(), runtime.GOMAXPROCS(0))
	if err != nil {
		return nil, nil, fmt.Errorf("wire authn: %w", err)
	}
	classes := map[string]*authn.TokenClass{}
	for aud, secret := range map[string]config.Secret{
		authn.AudienceMarketplace: cfg.JWTMarketplaceKeys,
		authn.AudienceStaff:       cfg.JWTStaffKeys,
	} {
		ring, err := keyRing(ctx, aud, secret, logger)
		if err != nil {
			return nil, nil, err
		}
		if classes[aud], err = authn.NewTokenClass(aud, ring, clk); err != nil {
			return nil, nil, fmt.Errorf("wire authn: %w", err)
		}
	}
	users, err := authn.NewService(database, passwords, classes[authn.AudienceMarketplace], pepper, clk, logger, metrics, onRevoke)
	if err != nil {
		return nil, nil, fmt.Errorf("wire authn: %w", err)
	}
	staff, err := authn.NewStaffService(database, passwords, classes[authn.AudienceStaff], pepper, clk, logger, metrics)
	if err != nil {
		return nil, nil, fmt.Errorf("wire authn: %w", err)
	}
	return users, staff, nil
}

// localWebBaseURL is the web origin of mailed links where none is
// configured: local and test only, where the mail itself is dropped
// (config.ServeMailErrors requires one elsewhere).
const localWebBaseURL = "http://localhost:3000"

// wireAccounts builds the account flows (ADR-0032) and the verification
// flows (ADR-0033): an insert-only River client for the jobs they queue
// in their transactions, the SMTP relay, or where none is configured a
// sender that drops the mail, and the İleti Merkezi account, or where
// none is configured a sender that drops the text.
func wireAccounts(cfg config.Config, database *db.DB, users *authn.Service, logger *slog.Logger,
) (*authn.Accounts, *authn.Verifications, error) {
	queue, err := jobs.NewClient(database.Pool(), jobs.Config{Logger: logger})
	if err != nil {
		return nil, nil, fmt.Errorf("wire accounts: %w", err)
	}
	var sender interface {
		Send(ctx context.Context, m mailer.Message) error
	}
	web := cfg.Mail.WebBaseURL
	if cfg.Mail.SMTPHost == "" {
		sender = droppedMail{logger: logger}
		if web == "" {
			web = localWebBaseURL
		}
	} else {
		s, err := smtp.New(smtpConfig(cfg))
		if err != nil {
			return nil, nil, fmt.Errorf("wire accounts: %w", err)
		}
		sender = s
	}
	accounts, err := authn.NewAccounts(users, queue, sender, web, cfg.Mail.RegistrationsPerHour)
	if err != nil {
		return nil, nil, fmt.Errorf("wire accounts: %w", err)
	}
	var texts interface {
		Send(ctx context.Context, m sms.Message) error
	}
	switch {
	case cfg.SMS.Driver == config.SMSDriverLog:
		texts = loggedText{logger: logger}
	case cfg.SMS.Configured():
		c, err := iletimerkezi.New(iletimerkezi.Config{
			Key: cfg.SMS.APIKey.Reveal(), Hash: cfg.SMS.APIHash.Reveal(), Sender: cfg.SMS.Sender,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("wire verifications: %w", err)
		}
		texts = c
	default:
		texts = droppedText{logger: logger}
	}
	verifications, err := authn.NewVerifications(users, queue, sender, texts, authn.VerificationCaps{
		TextsPerHour: cfg.SMS.PerHour, MailsPerHour: cfg.Verification.MailsPerHour,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("wire verifications: %w", err)
	}
	return accounts, verifications, nil
}

// smtpConfig is the relay configuration of cfg. Encryption is required
// everywhere but local and test (ADR-0032 Karar 4).
func smtpConfig(cfg config.Config) smtp.Config {
	return smtp.Config{
		Host: cfg.Mail.SMTPHost, Port: cfg.Mail.SMTPPort,
		Username: cfg.Mail.SMTPUsername, Password: cfg.Mail.SMTPPassword.Reveal(),
		From: cfg.Mail.From, RequireTLS: cfg.Env != config.EnvLocal && cfg.Env != config.EnvTest,
	}
}

// droppedMail stands in for the relay in local and test without one. It
// logs that a mail was dropped and its subject, never the recipient or the
// body: the body holds a live token.
type droppedMail struct {
	logger *slog.Logger
}

func (d droppedMail) Send(ctx context.Context, m mailer.Message) error {
	d.logger.WarnContext(ctx, "no SMTP relay configured; mail dropped", slog.String("subject", m.Subject))
	return nil
}

// droppedText stands in for the SMS provider in local and test without
// one. It logs that a text was dropped, never the number or the text: the
// text holds a live code and the number is personal data.
type droppedText struct {
	logger *slog.Logger
}

func (d droppedText) Send(ctx context.Context, _ sms.Message) error {
	d.logger.WarnContext(ctx, "no SMS provider configured; text message dropped")
	return nil
}

// loggedText is the log driver of KOALA_SMS_DRIVER (local, test and
// staging; config refuses it in production, ADR-0033): it sends nothing
// and logs only that a message went, never the number or the text, which
// holds a live code.
type loggedText struct {
	logger *slog.Logger
}

func (l loggedText) Send(ctx context.Context, _ sms.Message) error {
	l.logger.InfoContext(ctx, "text message sent by the log driver; nothing left the service")
	return nil
}

func keyRing(ctx context.Context, aud string, secret config.Secret, logger *slog.Logger) (tokenkeys.Ring, error) {
	if secret == "" {
		logger.WarnContext(ctx, "token signing keys not configured; using an ephemeral key, tokens die with the process",
			slog.String("audience", aud))
		return tokenkeys.Generate(aud), nil
	}
	ring, err := tokenkeys.Parse(secret.Reveal())
	if err != nil {
		return tokenkeys.Ring{}, fmt.Errorf("wire authn: %s keys: %w", aud, err)
	}
	return ring, nil
}

// loginPepper is KOALA_LOGIN_PEPPER, or outside staging and production
// when it is unset a per-process one (attempt targets then do not match
// across restarts).
func loginPepper(ctx context.Context, secret config.Secret, logger *slog.Logger) ([]byte, error) {
	if secret == "" {
		logger.WarnContext(ctx, "login pepper not configured; using an ephemeral one")
		return tokenkeys.GeneratePepper(), nil
	}
	pepper, err := tokenkeys.ParsePepper(secret.Reveal())
	if err != nil {
		return nil, fmt.Errorf("wire authn: login pepper: %w", err)
	}
	return pepper, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// realtimeSessions is the realtime layer's session check on the user
// session table (realtime.Sessions, ADR-0022 §2).
type realtimeSessions struct {
	users *authn.Service
}

func (s realtimeSessions) Active(ctx context.Context, p realtime.Principal) (bool, error) {
	userID, err := uuid.Parse(string(p.UserID))
	if err != nil {
		return false, nil //nolint:nilerr // a principal that names no user is not an active session (contract: no error)
	}
	sessionID, err := uuid.Parse(p.SessionID)
	if err != nil {
		return false, nil //nolint:nilerr // as above
	}
	active, err := s.users.SessionActive(ctx, userID, sessionID, p.DeviceID)
	if err != nil {
		return false, fmt.Errorf("realtime session check: %w", err)
	}
	return active, nil
}

// realtimeAuth authenticates the ticket request like any marketplace
// request: bearer token, live session, the session's device.
type realtimeAuth struct {
	users *authn.Service
}

func (a realtimeAuth) Principal(r *http.Request) (realtime.Principal, error) {
	ctx := r.Context()
	p, err := a.users.Authenticate(ctx, authn.BearerToken(r), httpx.DeviceIDFrom(ctx))
	if err != nil {
		return realtime.Principal{}, fmt.Errorf("realtime ticket: %w", err)
	}
	return realtime.Principal{UserID: realtime.UserID(p.UserID.String()), SessionID: p.SessionID.String(), DeviceID: p.DeviceKey}, nil
}

// noConversations answers the realtime layer until messaging lands (Faz
// 4): nobody is a participant of anything, so typing is
// CONVERSATION_NOT_FOUND and no conversation event has a recipient.
type noConversations struct{}

func (noConversations) IsParticipant(context.Context, realtime.ConversationID, realtime.UserID) (bool, error) {
	return false, nil
}

func (noConversations) Participants(context.Context, realtime.ConversationID) ([]realtime.UserID, error) {
	return nil, nil
}
