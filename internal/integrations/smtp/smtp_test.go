package smtp_test

import (
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/integrations/smtp"
	"github.com/is-koala/koala-api/internal/platform/fakes"
	"github.com/is-koala/koala-api/internal/platform/mailer"
)

func message() mailer.Message {
	return mailer.Message{
		To: "ayse@example.com", Subject: "Koala test subject",
		Text: "plain body line\n", HTML: "<p>html body</p>",
	}
}

// The real client against the fake relay: the message arrives with its
// envelope, its subject and both alternatives.
func TestSendDeliversThroughTheRelay(t *testing.T) {
	t.Parallel()

	relay := fakes.NewSMTP(t)
	s, err := smtp.New(smtp.Config{Host: relay.Host(), Port: relay.Port(), From: "Koala <no-reply@iskoala.com>"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(t.Context(), message()); err != nil {
		t.Fatal(err)
	}
	msgs := relay.Messages()
	if len(msgs) != 1 {
		t.Fatalf("relay got %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.From != "no-reply@iskoala.com" || len(m.To) != 1 || m.To[0] != "ayse@example.com" {
		t.Errorf("envelope = %s -> %v", m.From, m.To)
	}
	for _, want := range []string{"Subject: Koala test subject", "text/plain", "plain body line", "text/html", "html body"} {
		if !strings.Contains(m.Data, want) {
			t.Errorf("message lacks %q:\n%s", want, m.Data)
		}
	}
}

// A relay that offers no STARTTLS fails a sender that requires TLS: the
// message is never sent in plain text where serve requires encryption.
func TestRequiredTLSIsNotSkipped(t *testing.T) {
	t.Parallel()

	relay := fakes.NewSMTP(t)
	s, err := smtp.New(smtp.Config{Host: relay.Host(), Port: relay.Port(), From: "no-reply@iskoala.com", RequireTLS: true})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Send(t.Context(), message())
	if err == nil {
		t.Fatal("Send without STARTTLS succeeded although TLS is required")
	}
	if strings.Contains(err.Error(), "ayse@example.com") {
		t.Errorf("error %q echoes the recipient", err)
	}
	if n := len(relay.Messages()); n != 0 {
		t.Errorf("relay got %d messages in plain text", n)
	}
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]smtp.Config{
		"no host":           {From: "no-reply@iskoala.com"},
		"no sender":         {Host: "mail.example.com"},
		"bad sender":        {Host: "mail.example.com", From: "not an address"},
		"user, no password": {Host: "mail.example.com", From: "no-reply@iskoala.com", Username: "koala"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := smtp.New(cfg); err == nil {
				t.Errorf("New(%+v) succeeded", cfg)
			}
		})
	}
}
