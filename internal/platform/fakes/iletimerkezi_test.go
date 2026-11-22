package fakes_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/fakes"
)

// smsOrder is the order part of a send request; recipients is the whole
// recipients member, its key included.
func smsOrder(sender, iys, text, recipients string) string {
	return `"order":{"sender":"` + sender + `"` + iys + `,"message":{"text":"` + text + `",` + recipients + `}}`
}

// smsBody is a send request of the fake's account key with the
// provider's key names.
func smsBody(hash, order string) string {
	return `{"request":{"authentication":{"key":"key-1","hash":"` + hash + `"},` + order + `}}`
}

// recipientsOf is the recipients member with the provider's spelling.
func recipientsOf(number string) string {
	return `"` + "rece" + "ipents" + `":{"number":["` + number + `"]}`
}

const okText = "Koala doğrulama kodunuz: 042913"

// statusCode reads response.status.code of an answer.
func statusCode(t *testing.T, body map[string]any) any {
	t.Helper()
	resp, _ := body["response"].(map[string]any)
	status, _ := resp["status"].(map[string]any)
	return status["code"]
}

func TestIletiMerkeziAcceptsAnAuthenticatedMessage(t *testing.T) {
	t.Parallel()
	f := fakes.NewIletiMerkezi(t, "key-1", "hash-1", "KOALA")
	status, body := call(t, f.Client(), http.MethodPost, f.URL()+f.SendPath(),
		http.Header{"Content-Type": {"application/json"}},
		smsBody("hash-1", smsOrder("KOALA", `,"iys":"0","iysList":""`, okText, recipientsOf("5321234567"))))
	m := decode(t, body)
	if status != http.StatusOK || statusCode(t, m) != "200" {
		t.Fatalf("status %d, body %s", status, body)
	}
	msgs := f.Messages()
	if len(msgs) != 1 || msgs[0].Text != okText || msgs[0].Numbers[0] != "5321234567" ||
		msgs[0].Sender != "KOALA" || msgs[0].IYS != "0" || msgs[0].OrderID == "" {
		t.Fatalf("Messages = %+v", msgs)
	}
}

// Every refusal the provider documents for an order, each with its own
// code and nothing accepted.
func TestIletiMerkeziRefuses(t *testing.T) {
	t.Parallel()
	iys := `,"iys":"0","iysList":""`
	ok := recipientsOf("5321234567")
	for name, tc := range map[string]struct {
		body   string
		reject string
		want   string
	}{
		"foreign credentials": {body: smsBody("other", smsOrder("KOALA", iys, okText, ok)), want: "401"},
		"not JSON":            {body: "<request/>", want: "400"},
		"no iys flag":         {body: smsBody("hash-1", smsOrder("KOALA", "", okText, ok)), want: "468"},
		"an empty date list": {
			body: smsBody("hash-1", smsOrder("KOALA", iys+`,"sendDateTime":[]`, okText, ok)), want: "457",
		},
		"a malformed date": {
			body: smsBody("hash-1", smsOrder("KOALA", iys+`,"sendDateTime":"2026-11-22 10:00"`, okText, ok)), want: "457",
		},
		"no sender":         {body: smsBody("hash-1", smsOrder("", iys, okText, ok)), want: "459"},
		"sender too long":   {body: smsBody("hash-1", smsOrder("KOALAKOALAKO", iys, okText, ok)), want: "460"},
		"unapproved sender": {body: smsBody("hash-1", smsOrder("OTHER", iys, okText, ok)), want: "450"},
		"empty text":        {body: smsBody("hash-1", smsOrder("KOALA", iys, "", ok)), want: "454"},
		"over seven parts":  {body: smsBody("hash-1", smsOrder("KOALA", iys, strings.Repeat("ş", 7*148+1), ok)), want: "453"},
		"the English key": {
			body: smsBody("hash-1", smsOrder("KOALA", iys, okText, `"recipients":{"number":["5321234567"]}`)), want: "452",
		},
		"a malformed number": {body: smsBody("hash-1", smsOrder("KOALA", iys, okText, recipientsOf("02121234567"))), want: "452"},
		"rejected number": {
			body: smsBody("hash-1", smsOrder("KOALA", iys, okText, recipientsOf("5329999999"))), reject: "5329999999", want: "452",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := fakes.NewIletiMerkezi(t, "key-1", "hash-1", "KOALA")
			if tc.reject != "" {
				f.RejectNumber(tc.reject)
			}
			_, body := call(t, f.Client(), http.MethodPost, f.URL()+f.SendPath(),
				http.Header{"Content-Type": {"application/json"}}, tc.body)
			if got := statusCode(t, decode(t, body)); got != tc.want {
				t.Fatalf("code = %v, want %s: %s", got, tc.want, body)
			}
			if len(f.Messages()) != 0 {
				t.Fatalf("a refused message was accepted: %+v", f.Messages())
			}
		})
	}
}
