package fakes_test

import (
	"net/http"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/fakes"
)

// smsBody is a send request with the provider's key names.
func smsBody(key, hash, number string) string {
	return `{"request":{"authentication":{"key":"` + key + `","hash":"` + hash + `"},` +
		`"order":{"sender":"KOALA","sendDateTime":[],"iys":"0","iysList":"",` +
		`"message":{"text":"Koala doğrulama kodunuz: 042913","` + "rece" + "ipents" + `":{"number":["` + number + `"]}}}}}`
}

// statusCode reads response.status.code of an answer.
func statusCode(t *testing.T, body map[string]any) any {
	t.Helper()
	resp, _ := body["response"].(map[string]any)
	status, _ := resp["status"].(map[string]any)
	return status["code"]
}

func TestIletiMerkeziAcceptsAnAuthenticatedMessage(t *testing.T) {
	t.Parallel()
	f := fakes.NewIletiMerkezi(t, "key-1", "hash-1")
	status, body := call(t, f.Client(), http.MethodPost, f.URL()+f.SendPath(),
		http.Header{"Content-Type": {"application/json"}}, smsBody("key-1", "hash-1", "5321234567"))
	m := decode(t, body)
	if status != http.StatusOK || statusCode(t, m) != "200" {
		t.Fatalf("status %d, body %s", status, body)
	}
	msgs := f.Messages()
	if len(msgs) != 1 || msgs[0].Text != "Koala doğrulama kodunuz: 042913" || msgs[0].Numbers[0] != "5321234567" ||
		msgs[0].Sender != "KOALA" || msgs[0].IYS != "0" || msgs[0].OrderID == "" {
		t.Fatalf("Messages = %+v", msgs)
	}
}

func TestIletiMerkeziRefuses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body   func(*fakes.IletiMerkezi) string
		reject string
		want   string
	}{
		"foreign credentials": {body: func(*fakes.IletiMerkezi) string { return smsBody("key-1", "other", "5321234567") }, want: "401"},
		"rejected number": {
			body: func(*fakes.IletiMerkezi) string { return smsBody("key-1", "hash-1", "5329999999") }, reject: "5329999999", want: "452",
		},
		"not JSON": {body: func(*fakes.IletiMerkezi) string { return "<request/>" }, want: "400"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := fakes.NewIletiMerkezi(t, "key-1", "hash-1")
			if tc.reject != "" {
				f.RejectNumber(tc.reject)
			}
			_, body := call(t, f.Client(), http.MethodPost, f.URL()+f.SendPath(),
				http.Header{"Content-Type": {"application/json"}}, tc.body(f))
			if got := statusCode(t, decode(t, body)); got != tc.want {
				t.Fatalf("code = %v, want %s: %s", got, tc.want, body)
			}
			if len(f.Messages()) != 0 {
				t.Fatalf("a refused message was accepted: %+v", f.Messages())
			}
		})
	}
}
