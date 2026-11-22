package iletimerkezi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/integrations/iletimerkezi"
	"github.com/is-koala/koala-api/internal/platform/fakes"
	"github.com/is-koala/koala-api/internal/platform/sms"
)

const (
	testKey  = "test-key"
	testHash = "test-hash-secret"
	phone    = "+905321234567"
)

func newClient(t *testing.T, f *fakes.IletiMerkezi) *iletimerkezi.Client {
	t.Helper()
	c, err := iletimerkezi.New(iletimerkezi.Config{
		BaseURL: f.URL(), Key: testKey, Hash: testHash, Sender: "KOALA", HTTPClient: f.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The real client against the fake: the message arrives with the
// provider's wire names, the ten national digits, the sender, iys 0, and
// the credentials in the body only.
func TestSendDeliversThroughTheProvider(t *testing.T) {
	t.Parallel()
	f := fakes.NewIletiMerkezi(t, testKey, testHash)
	if err := newClient(t, f).Send(t.Context(), sms.Message{To: phone, Text: "Koala doğrulama kodunuz: 042913"}); err != nil {
		t.Fatal(err)
	}
	msgs := f.Messages()
	if len(msgs) != 1 {
		t.Fatalf("provider accepted %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Text != "Koala doğrulama kodunuz: 042913" || len(m.Numbers) != 1 || m.Numbers[0] != "5321234567" ||
		m.Sender != "KOALA" || m.IYS != "0" {
		t.Errorf("message = %+v", m)
	}
	reqs := f.RequestsTo(http.MethodPost, f.SendPath())
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if len(r.Query) != 0 {
		t.Errorf("the request carries a query %v; credentials belong in the body", r.Query)
	}
	var body struct {
		Request struct {
			Order struct {
				Message map[string]json.RawMessage `json:"message"`
			} `json:"order"`
		} `json:"request"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body.Request.Order.Message[iletimerkezi.RecipientsKey]; !ok || iletimerkezi.RecipientsKey != "rece"+"ipents" {
		t.Errorf("message keys %v lack the provider's recipients key", body.Request.Order.Message)
	}
}

// The provider's answer decides: a refused recipient or message is
// undeliverable and stops the job; an operational refusal or an
// unreadable answer is retried. Neither error carries the number, the
// text or the credentials. The code is read as a string and as a number.
func TestSendClassifiesTheAnswer(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		answer      fakes.Response
		undelivered bool
		ok          bool
	}{
		"accepted, numeric code":    {answer: fakes.IletiMerkeziStatus(fakes.IletiMerkeziOK, true), ok: true},
		"recipient refused":         {answer: fakes.IletiMerkeziStatus(fakes.IletiMerkeziRecipients, false), undelivered: true},
		"recipient refused numeric": {answer: fakes.IletiMerkeziStatus(fakes.IletiMerkeziRecipients, true), undelivered: true},
		"text too long":             {answer: fakes.IletiMerkeziStatus(fakes.IletiMerkeziTooLong, false), undelivered: true},
		"empty text":                {answer: fakes.IletiMerkeziStatus(fakes.IletiMerkeziEmptyText, false), undelivered: true},
		"no balance":                {answer: fakes.IletiMerkeziStatus(fakes.IletiMerkeziNoBalance, false)},
		"sender not approved":       {answer: fakes.IletiMerkeziStatus(fakes.IletiMerkeziBadSender, false)},
		"provider down":             {answer: fakes.Response{Status: http.StatusBadGateway, Body: []byte("<html>bad gateway</html>")}},
		"dropped":                   {answer: fakes.Response{Drop: true}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := fakes.NewIletiMerkezi(t, testKey, testHash)
			f.Enqueue(http.MethodPost, f.SendPath(), tc.answer)
			err := newClient(t, f).Send(t.Context(), sms.Message{To: phone, Text: "Koala doğrulama kodunuz: 042913"})
			if tc.ok {
				if err != nil {
					t.Fatalf("Send = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Send succeeded, want an error")
			}
			if got := errors.Is(err, sms.ErrUndeliverable); got != tc.undelivered {
				t.Errorf("Send = %v: undeliverable %v, want %v", err, got, tc.undelivered)
			}
			for _, secret := range []string{"5321234567", "042913", testHash, testKey} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q carries %q", err, secret)
				}
			}
		})
	}
}

// Wrong credentials are an operational fault, retried, not a refused
// recipient.
func TestSendWithForeignCredentials(t *testing.T) {
	t.Parallel()
	f := fakes.NewIletiMerkezi(t, testKey, "another-hash")
	err := newClient(t, f).Send(t.Context(), sms.Message{To: phone, Text: "x"})
	if err == nil || errors.Is(err, sms.ErrUndeliverable) {
		t.Fatalf("Send with foreign credentials = %v, want a retried error", err)
	}
}

// A number outside Türkiye never reaches the provider.
func TestSendOnlyToTurkishNumbers(t *testing.T) {
	t.Parallel()
	f := fakes.NewIletiMerkezi(t, testKey, testHash)
	for _, to := range []string{"+447700900123", "+9053212345", "05321234567", ""} {
		err := newClient(t, f).Send(t.Context(), sms.Message{To: to, Text: "x"})
		if !errors.Is(err, sms.ErrUndeliverable) {
			t.Errorf("Send to %q = %v, want undeliverable", to, err)
		}
	}
	if n := len(f.Requests()); n != 0 {
		t.Errorf("the provider got %d requests", n)
	}
}

func TestNewChecksTheConfiguration(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]iletimerkezi.Config{
		"no key":    {Hash: "h", Sender: "KOALA"},
		"no hash":   {Key: "k", Sender: "KOALA"},
		"no sender": {Key: "k", Hash: "h"},
		"bad base":  {Key: "k", Hash: "h", Sender: "KOALA", BaseURL: "ftp://example.com"},
		"no host":   {Key: "k", Hash: "h", Sender: "KOALA", BaseURL: "https://"},
	} {
		if _, err := iletimerkezi.New(cfg); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
	if _, err := iletimerkezi.New(iletimerkezi.Config{Key: "k", Hash: "h", Sender: "KOALA"}); err != nil {
		t.Errorf("the default base URL: %v", err)
	}
}
