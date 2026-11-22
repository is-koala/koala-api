package authn

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// A number is normalized to E.164 however people type it: +90, a leading
// 0, 0090, spaces, dashes, dots and parentheses (ADR-0033). What is not a
// number is refused, not guessed.
func TestNormalizePhone(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"+905321234567":       "+905321234567",
		"+90 532 123 45 67":   "+905321234567",
		"0532 123 45 67":      "+905321234567",
		"05321234567":         "+905321234567",
		"5321234567":          "+905321234567",
		"905321234567":        "+905321234567",
		"00905321234567":      "+905321234567",
		"(0532) 123-45-67":    "+905321234567",
		"0532.123.45.67":      "+905321234567",
		" +90-532-123-45-67 ": "+905321234567",
		"+44 7700 900123":     "+447700900123",
		"+90 212 123 45 67":   "+902121234567",
	} {
		if got, ok := NormalizePhone(in); !ok || got != want {
			t.Errorf("NormalizePhone(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "abc", "+", "+0905321234567", "532123", "0532 123 45 6x", "+90 532 123 45 67 ext 3",
		"++905321234567", "90532+1234567", "+1234567"} {
		if got, ok := NormalizePhone(in); ok {
			t.Errorf("NormalizePhone(%q) = %q, want refused", in, got)
		}
	}
}

// Only Turkish mobile numbers get a text: a landline, a premium or
// shared-cost range and every other country are refused before anything
// is sent, the SMS pumping defense (ADR-0033). The rule sees a real case
// on each side.
func TestTextableNumber(t *testing.T) {
	t.Parallel()

	for n, want := range map[string]bool{
		"+905321234567": true,
		"+905051234567": true,
		"+902121234567": false, // İstanbul landline
		"+908501234567": false, // shared-cost 0850
		"+909001234567": false, // premium 0900
		"+90532123456":  false, // a digit short
		"+447700900123": false,
		"+12025550123":  false,
		"+882123456789": false, // international networks, a pumping favorite
	} {
		if got := TextableNumber(n); got != want {
			t.Errorf("TextableNumber(%q) = %v, want %v", n, got, want)
		}
	}
}

// The target is checked for its channel; an address's budget key is its
// mailbox, a number's its E.164 form, so every spelling of one target
// is one budget (ADR-0032 Karar 6, ADR-0033).
func TestVerificationTarget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ channel, raw, target, budget string }{
		{ChannelEmail, " Ayse+Koala@Example.com ", "ayse+koala@example.com", "ayse@example.com"},
		{ChannelEmail, "a.y.s.e@gmail.com", "a.y.s.e@gmail.com", "ayse@gmail.com"},
		{ChannelPhone, "0532 123 45 67", "+905321234567", "+905321234567"},
	} {
		target, budget, err := verificationTarget(tc.channel, tc.raw)
		if err != nil || target != tc.target || budget != tc.budget {
			t.Errorf("verificationTarget(%s, %q) = %q, %q, %v; want %q, %q", tc.channel, tc.raw, target, budget, err, tc.target, tc.budget)
		}
	}
	for _, tc := range []struct{ channel, raw string }{
		{ChannelEmail, "not an address"},
		{ChannelEmail, "+905321234567"},
		{ChannelPhone, "ayse@example.com"},
		{ChannelPhone, "+447700900123"},
		{ChannelPhone, "0212 123 45 67"},
	} {
		_, _, err := verificationTarget(tc.channel, tc.raw)
		e := apperr.From(err)
		if e.Kind != apperr.KindInvalid || len(e.Fields) != 1 || e.Fields[0].Pointer != "/target" {
			t.Errorf("verificationTarget(%s, %q) = %v, want REQUEST_INVALID at /target", tc.channel, tc.raw, err)
		}
	}
}

func TestMaskTarget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ channel, target, want string }{
		{ChannelEmail, "ayse@example.com", "a***@example.com"},
		{ChannelEmail, "şule@example.com", "ş***@example.com"},
		{ChannelPhone, "+905321234567", "+90******4567"},
	} {
		if got := maskTarget(tc.channel, tc.target); got != tc.want {
			t.Errorf("maskTarget(%s, %q) = %q, want %q", tc.channel, tc.target, got, tc.want)
		}
	}
}

// A code is six digits over the whole space: a leading zero comes up
// (Java's 100000 + nextInt(900000) never draws one), and every draw is
// six decimal digits (USR-008).
func TestCodeUsesTheWholeSpace(t *testing.T) {
	t.Parallel()

	six := regexp.MustCompile(`^\d{6}$`)
	leadingZero := false
	for range 20000 {
		c, err := newVerificationCode()
		if err != nil {
			t.Fatal(err)
		}
		if !six.MatchString(c) {
			t.Fatalf("code %q is not six digits", c)
		}
		leadingZero = leadingZero || c[0] == '0'
	}
	// P(no leading zero in 20000 uniform draws) = 0.9^20000.
	if !leadingZero {
		t.Error("no code of 20000 started with 0: the space excludes it (USR-008)")
	}
}

// The stored digest is keyed: it is not the code's plain SHA-256, another
// key or another verification gives another digest, and only the right
// code matches, in constant time (ADR-0033).
func TestCodeDigestIsKeyed(t *testing.T) {
	t.Parallel()

	a := &Verifications{codeKey: []byte("key-a-key-a-key-a-key-a-key-a-32")}
	b := &Verifications{codeKey: []byte("key-b-key-b-key-b-key-b-key-b-32")}
	id, other := uuid.New(), uuid.New()
	d := a.codeDigest(id, "042913")
	plain := sha256.Sum256([]byte("042913"))
	if bytes.Equal(d, plain[:]) {
		t.Error("USR-008: the digest is the code's plain SHA-256")
	}
	if bytes.Equal(d, b.codeDigest(id, "042913")) || bytes.Equal(d, a.codeDigest(other, "042913")) {
		t.Error("USR-008: the digest does not depend on the key and the verification")
	}
	if !a.codeMatches(id, d, "042913") || a.codeMatches(id, d, "042914") || a.codeMatches(id, nil, "042913") {
		t.Error("USR-008: codeMatches does not tell the right code from a wrong one or a missing digest")
	}
}

// The code mail goes to an address nobody has verified yet, so it carries
// the code and nothing the account holder typed, not even the account's
// name (ADR-0032 Karar 3, ADR-0033).
func TestCodeMailNamesNobody(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"verification_code.txt", "verification_code.html"} {
		body, err := fs.ReadFile(mailTemplates, "mail/"+name)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{".FirstName", ".LastName", ".Email", ".Changed"} {
			if strings.Contains(string(body), field) {
				t.Errorf("%s uses %s; an unverified address's mail carries only the code", name, field)
			}
		}
		if !strings.Contains(string(body), ".Code") {
			t.Errorf("%s does not carry the code", name)
		}
	}
	mails, err := parseMailTemplates(mailVerificationCode, mailContactChanged)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mails.Render(mailVerificationCode, "yeni@example.com", codeMailData{Code: "042913"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Text, "042913") || !strings.Contains(msg.HTML, "042913") || strings.Contains(msg.Subject, "042913") {
		t.Errorf("the code mail: subject %q, text %q", msg.Subject, msg.Text)
	}
}

// The text of a code fits one segment in the encoding Turkish letters
// need (UCS-2, 70 characters).
func TestCodeTextIsOneSegment(t *testing.T) {
	t.Parallel()

	if n := len([]rune(strings.Replace(textCode, "%s", "042913", 1))); n > 70 {
		t.Errorf("the code text is %d characters, over one UCS-2 segment", n)
	}
}

// The list cursor round-trips, and a malformed or foreign one is
// CURSOR_INVALID.
func TestListCursor(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 22, 9, 0, 0, 123456789, time.UTC)
	id := uuid.New()
	for _, newer := range []bool{false, true} {
		c := listCursor{newer: newer, at: at, id: id}.encode()
		got, err := decodeCursor(*c)
		if err != nil || got.newer != newer || !got.at.Equal(at) || got.id != id {
			t.Errorf("decodeCursor(encode(%v)) = %+v, %v", newer, got, err)
		}
	}
	for _, bad := range []string{"%%%", "dnJmMXxufDF8eA", "eHx5", "", "dnJmMnxufDF8MDE5MmYzYzQtN2IxYS03Y2MwLTlhNTEtM2YyYjhlNGQ2YTEw"} {
		_, err := decodeCursor(bad)
		if e := apperr.From(err); e.Code != CodeCursorInvalid || e.Kind != apperr.KindInvalid {
			t.Errorf("decodeCursor(%q) = %v, want CURSOR_INVALID", bad, err)
		}
	}
}

// RetryAfter reads the wait of a resend refusal through wrapping, and
// the refusal is the contract's 429 code.
func TestRetryAfter(t *testing.T) {
	t.Parallel()

	err := error(&resendError{err: apperr.New(apperr.KindRateLimited, CodeVerificationResendTooSoon), wait: 42 * time.Second})
	wrapped := errors.Join(errors.New("context"), err)
	if wait, ok := RetryAfter(wrapped); !ok || wait != 42*time.Second {
		t.Errorf("RetryAfter = %v, %v", wait, ok)
	}
	if e := apperr.From(wrapped); e.Code != CodeVerificationResendTooSoon || e.Kind != apperr.KindRateLimited {
		t.Errorf("apperr.From = %v", e)
	}
	if _, ok := RetryAfter(errors.New("other")); ok {
		t.Error("RetryAfter found a wait in an unrelated error")
	}
}
