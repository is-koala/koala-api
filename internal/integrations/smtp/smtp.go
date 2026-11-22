// Package smtp delivers mail through an SMTP relay with go-mail (plan
// §3.1, §3.3; ADR-0032). It is called from River jobs only, never inside
// a database transaction (CLAUDE.md "Transaction").
//
// Each Send dials, delivers one message and quits: mail is rare (a
// registration, a reset) and a held connection would only time out
// between them. Outside local and test the connection must be encrypted:
// implicit TLS on port 465, STARTTLS on any other port, and a relay that
// does not offer STARTTLS fails the send instead of getting the message in
// plain text.
package smtp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wneessen/go-mail"

	"github.com/is-koala/koala-api/internal/platform/mailer"
)

// DefaultPort is the submission port with STARTTLS (RFC 8314 §3.3).
const DefaultPort = 587

// implicitTLSPort is the submission port with implicit TLS (RFC 8314 §3.3).
const implicitTLSPort = 465

// timeout bounds the dial and each command; a River job's own timeout
// bounds the whole send.
const timeout = 15 * time.Second

// Config is a relay and the sender address.
type Config struct {
	Host string
	// Port is DefaultPort when 0.
	Port int
	// Username and Password authenticate the session; both empty means no
	// authentication (a local relay).
	Username string
	Password string
	// From is the sender, an RFC 5322 address with an optional name.
	From string
	// RequireTLS refuses a relay that offers no encryption. serve sets it
	// everywhere but local and test.
	RequireTLS bool
}

// Sender delivers messages through the relay.
type Sender struct {
	cfg  Config
	opts []mail.Option
}

// New checks cfg and returns the sender. Nothing is dialed until Send.
func New(cfg Config) (*Sender, error) {
	if cfg.Host == "" || cfg.From == "" {
		return nil, errors.New("smtp: host and sender are required")
	}
	if (cfg.Username == "") != (cfg.Password == "") {
		return nil, errors.New("smtp: username and password are set together")
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	policy := mail.TLSOpportunistic
	if cfg.RequireTLS {
		policy = mail.TLSMandatory
	}
	opts := []mail.Option{mail.WithPort(cfg.Port), mail.WithTimeout(timeout), mail.WithTLSPolicy(policy)}
	if cfg.Port == implicitTLSPort {
		opts = append(opts, mail.WithSSL())
	}
	if cfg.Username != "" {
		opts = append(opts, mail.WithSMTPAuth(mail.SMTPAuthAutoDiscover),
			mail.WithUsername(cfg.Username), mail.WithPassword(cfg.Password))
	}
	if _, err := mail.NewClient(cfg.Host, opts...); err != nil {
		return nil, fmt.Errorf("smtp: configure client: %w", err)
	}
	if err := mail.NewMsg().From(cfg.From); err != nil {
		return nil, fmt.Errorf("smtp: sender address: %w", err)
	}
	return &Sender{cfg: cfg, opts: opts}, nil
}

// Send delivers m. The error does not echo the recipient.
func (s *Sender) Send(ctx context.Context, m mailer.Message) error {
	msg := mail.NewMsg()
	if err := msg.From(s.cfg.From); err != nil {
		return fmt.Errorf("smtp: sender address: %w", err)
	}
	if err := msg.To(m.To); err != nil {
		return errors.New("smtp: the recipient is not a valid address")
	}
	msg.Subject(m.Subject)
	msg.SetDate()
	msg.SetMessageID()
	msg.SetBodyString(mail.TypeTextPlain, m.Text)
	msg.AddAlternativeString(mail.TypeTextHTML, m.HTML)
	client, err := mail.NewClient(s.cfg.Host, s.opts...)
	if err != nil {
		return fmt.Errorf("smtp: configure client: %w", err)
	}
	if err := client.DialAndSendWithContext(ctx, msg); err != nil {
		// A SendError's text lists the recipients, and a River job's
		// error is stored and logged: keep only the relay's verdict.
		var se *mail.SendError
		if errors.As(err, &se) {
			return fmt.Errorf("smtp: send: %s (reply %d %s, temporary %t)",
				se.Reason, se.ErrorCode(), se.EnhancedStatusCode(), se.IsTemp())
		}
		return fmt.Errorf("smtp: send: %w", err)
	}
	return nil
}
