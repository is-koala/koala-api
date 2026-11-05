package fakes_test

import (
	"net/http"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/fakes"
)

const jwtBearerForm = "grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Ajwt-bearer&assertion=signed.jwt.here"

// fcmToken gets an access token from the fake's token endpoint.
func fcmToken(t *testing.T, f *fakes.FCM) string {
	t.Helper()
	status, body := call(t, f.Client(), http.MethodPost, f.TokenURL(),
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, jwtBearerForm)
	if status != http.StatusOK {
		t.Fatalf("token: status %d, body %s", status, body)
	}
	m := decode(t, body)
	tok, _ := m["access_token"].(string)
	if tok == "" || m["token_type"] != "Bearer" {
		t.Fatalf("token response %v", m)
	}
	return tok
}

func send(t *testing.T, f *fakes.FCM, token, message string) (int, map[string]any) {
	t.Helper()
	h := http.Header{"Content-Type": {"application/json"}}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	status, body := call(t, f.Client(), http.MethodPost, f.URL()+f.SendPath(), h, `{"message":`+message+`}`)
	return status, decode(t, body)
}

// fcmErrorCode returns the FcmError code in a Google API error body.
func fcmErrorCode(t *testing.T, body map[string]any) string {
	t.Helper()
	e, _ := body["error"].(map[string]any)
	details, _ := e["details"].([]any)
	if len(details) != 1 {
		t.Fatalf("error body %v has no FcmError detail", body)
	}
	d, _ := details[0].(map[string]any)
	if d["@type"] != "type.googleapis.com/google.firebase.fcm.v1.FcmError" {
		t.Fatalf("detail type %v", d["@type"])
	}
	code, _ := d["errorCode"].(string)
	return code
}

func TestFCMAcceptsAnAuthenticatedMessage(t *testing.T) {
	t.Parallel()
	f := fakes.NewFCM(t, "koala-test")
	tok := fcmToken(t, f)

	status, body := send(t, f, tok,
		`{"token":"device-1","notification":{"title":"Yeni iş","body":"Kadıköy"},"data":{"jobId":"42"}}`)
	if status != http.StatusOK || body["name"] != "projects/koala-test/messages/1" {
		t.Fatalf("status %d, body %v", status, body)
	}
	msgs := f.Messages()
	if len(msgs) != 1 {
		t.Fatalf("Messages = %+v", msgs)
	}
	m := msgs[0]
	if m.Token != "device-1" || m.Notification == nil || m.Notification.Title != "Yeni iş" || m.Data["jobId"] != "42" {
		t.Fatalf("message %+v", m)
	}
	if got := f.Requests()[1].Header.Get("Authorization"); got != "Bearer "+tok {
		t.Fatalf("recorded Authorization %q", got)
	}
}

func TestFCMRefusesUnauthenticatedSends(t *testing.T) {
	t.Parallel()
	for name, token := range map[string]string{"no token": "", "token it did not issue": "made-up"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := fakes.NewFCM(t, "koala-test")
			status, body := send(t, f, token, `{"token":"device-1"}`)
			e, _ := body["error"].(map[string]any)
			if status != http.StatusUnauthorized || e["status"] != "UNAUTHENTICATED" {
				t.Fatalf("status %d, body %v", status, body)
			}
			if len(f.Messages()) != 0 {
				t.Fatal("an unauthenticated message was accepted")
			}
		})
	}
}

func TestFCMRefusesABadTokenGrant(t *testing.T) {
	t.Parallel()
	f := fakes.NewFCM(t, "koala-test")
	status, body := call(t, f.Client(), http.MethodPost, f.TokenURL(), nil, "grant_type=password")
	if status != http.StatusBadRequest || decode(t, body)["error"] != "invalid_grant" {
		t.Fatalf("status %d, body %s", status, body)
	}
}

func TestFCMRefusesInvalidMessages(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"two targets":       `{"token":"d","topic":"jobs"}`,
		"no target":         `{"data":{"a":"b"}}`,
		"non-string data":   `{"token":"d","data":{"jobId":42}}`,
		"message not JSON":  `"text"`,
		"message is absent": `null`,
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := fakes.NewFCM(t, "koala-test")
			status, body := send(t, f, fcmToken(t, f), msg)
			if status != http.StatusBadRequest || fcmErrorCode(t, body) != string(fakes.FCMInvalidArgument) {
				t.Fatalf("status %d, body %v", status, body)
			}
		})
	}
}

func TestFCMFailToken(t *testing.T) {
	t.Parallel()
	f := fakes.NewFCM(t, "koala-test")
	tok := fcmToken(t, f)
	f.FailToken("uninstalled", fakes.FCMUnregistered)

	status, body := send(t, f, tok, `{"token":"uninstalled"}`)
	if status != http.StatusNotFound || fcmErrorCode(t, body) != "UNREGISTERED" {
		t.Fatalf("status %d, body %v", status, body)
	}
	if status, _ := send(t, f, tok, `{"token":"installed"}`); status != http.StatusOK {
		t.Fatalf("another device: status %d", status)
	}
	if status, _ := send(t, f, tok, `{"topic":"jobs"}`); status != http.StatusOK {
		t.Fatalf("topic: status %d", status)
	}
	if n := len(f.Messages()); n != 2 {
		t.Fatalf("accepted %d messages, want 2", n)
	}
}

func TestFCMEnqueuedResponsesComeFirst(t *testing.T) {
	t.Parallel()
	f := fakes.NewFCM(t, "koala-test")
	tok := fcmToken(t, f)
	f.Enqueue(http.MethodPost, f.SendPath(), f.ErrorResponse(fakes.FCMUnavailable))

	status, body := send(t, f, tok, `{"token":"device-1"}`)
	if status != http.StatusServiceUnavailable || fcmErrorCode(t, body) != "UNAVAILABLE" {
		t.Fatalf("first send: status %d, body %v", status, body)
	}
	if status, _ := send(t, f, tok, `{"token":"device-1"}`); status != http.StatusOK {
		t.Fatalf("second send: status %d", status)
	}
	if len(f.Messages()) != 1 || len(f.RequestsTo(http.MethodPost, f.SendPath())) != 2 {
		t.Fatalf("Messages %d, send requests %d", len(f.Messages()), len(f.RequestsTo(http.MethodPost, f.SendPath())))
	}
}

func TestFCMErrorResponses(t *testing.T) {
	t.Parallel()
	f := fakes.NewFCM(t, "koala-test")
	cases := []struct {
		code       fakes.FCMErrorCode
		httpStatus int
		status     string
	}{
		{fakes.FCMUnregistered, http.StatusNotFound, "NOT_FOUND"},
		{fakes.FCMInvalidArgument, http.StatusBadRequest, "INVALID_ARGUMENT"},
		{fakes.FCMQuotaExceeded, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED"},
		{fakes.FCMUnavailable, http.StatusServiceUnavailable, "UNAVAILABLE"},
		{fakes.FCMInternal, http.StatusInternalServerError, "INTERNAL"},
		{fakes.FCMSenderIDMismatch, http.StatusForbidden, "PERMISSION_DENIED"},
		{fakes.FCMThirdPartyAuthError, http.StatusUnauthorized, "UNAUTHENTICATED"},
	}
	for _, tc := range cases {
		resp := f.ErrorResponse(tc.code)
		body := decode(t, resp.Body)
		e, _ := body["error"].(map[string]any)
		if resp.Status != tc.httpStatus || e["status"] != tc.status || e["code"] != float64(tc.httpStatus) ||
			fcmErrorCode(t, body) != string(tc.code) {
			t.Fatalf("%s: status %d, body %v", tc.code, resp.Status, body)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("unknown code did not panic")
		}
	}()
	f.ErrorResponse("NOT_A_CODE")
}
