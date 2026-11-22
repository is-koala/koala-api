package authn

import (
	"io/fs"
	"net/netip"
	"strings"
	"testing"
	"unicode/utf8"
)

// The budget and the rate limit count mailboxes: every sub-address and,
// at Gmail, every dotting of one mailbox is one key (ADR-0032 Karar 6).
func TestMailboxKey(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"ayse@example.com":            "ayse@example.com",
		"ayse+koala@example.com":      "ayse@example.com",
		"ayse+a+b@example.com":        "ayse@example.com",
		"a.y.s.e@example.com":         "a.y.s.e@example.com", // dots count elsewhere
		"a.y.s.e+x@gmail.com":         "ayse@gmail.com",
		"ay.se@googlemail.com":        "ayse@gmail.com",
		"ayse@gmail.com":              "ayse@gmail.com",
		"no-at-sign":                  "no-at-sign",
		"quoted\"@\"part@example.com": "quoted\"@\"part@example.com",
	} {
		if got := mailboxKey(in); got != want {
			t.Errorf("mailboxKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// The registration link goes to an address nobody has verified yet, so it
// carries nothing the requester typed: its templates name the link only
// (ADR-0032 Karar 3). The reset, notice and welcome mails go to a verified
// account and may greet by name.
func TestRegistrationMailNamesNobody(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"registration_link.txt", "registration_link.html"} {
		body, err := fs.ReadFile(mailTemplates, "mail/"+name)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{".FirstName", ".LastName", ".Email"} {
			if strings.Contains(string(body), field) {
				t.Errorf("%s uses %s; an unverified address's mail carries nothing the requester typed", name, field)
			}
		}
		if !strings.Contains(string(body), ".Link") {
			t.Errorf("%s does not carry the link", name)
		}
	}
	// And the rendered mail: a name in the data does not reach it.
	mails, err := parseMailTemplates()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mails.Render(mailRegistrationLink, "a@example.com",
		mailData{FirstName: "Kurban<b>", LastName: "Soyad", Email: "a@example.com", Link: "https://iskoala.test/x?token=t"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.Text+msg.HTML+msg.Subject, "Kurban") {
		t.Error("the registration mail carries the requester's name")
	}
}

// The consent record keeps the resolved client address and a User-Agent
// bounded to 512 bytes, cut on a character boundary.
func TestConsentClient(t *testing.T) {
	t.Parallel()

	ip, agent := consentClient(netip.MustParseAddr("::ffff:192.0.2.7"), "  Koala/1.0 iOS  ")
	if ip == nil || *ip != "192.0.2.7" || agent == nil || *agent != "Koala/1.0 iOS" {
		t.Errorf("consentClient = %v, %v", ip, agent)
	}
	ip, agent = consentClient(netip.Addr{}, "")
	if ip != nil || agent != nil {
		t.Errorf("an absent address and agent = %v, %v; want nil", ip, agent)
	}
	long := strings.Repeat("ş", 300) // 600 bytes
	_, agent = consentClient(netip.Addr{}, long)
	if agent == nil {
		t.Fatal("a long User-Agent was dropped")
	}
	if n := len(*agent); n > maxUserAgentBytes || n < maxUserAgentBytes-1 || !utf8.ValidString(*agent) {
		t.Errorf("a long User-Agent became %d bytes (valid UTF-8 %t), want at most %d", n, utf8.ValidString(*agent), maxUserAgentBytes)
	}
}
