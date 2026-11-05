package fakes

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// FCMErrorCode is an FCM HTTP v1 error code (google.firebase.fcm.v1.FcmError).
type FCMErrorCode string

// The FCM error codes the Java backend classifies (FirebasePushService)
// and the Faz 5 client will.
const (
	FCMUnregistered        FCMErrorCode = "UNREGISTERED"
	FCMInvalidArgument     FCMErrorCode = "INVALID_ARGUMENT"
	FCMQuotaExceeded       FCMErrorCode = "QUOTA_EXCEEDED"
	FCMUnavailable         FCMErrorCode = "UNAVAILABLE"
	FCMInternal            FCMErrorCode = "INTERNAL"
	FCMSenderIDMismatch    FCMErrorCode = "SENDER_ID_MISMATCH"
	FCMThirdPartyAuthError FCMErrorCode = "THIRD_PARTY_AUTH_ERROR"
)

// fcmTokenPath is where the fake serves OAuth 2.0 tokens.
const fcmTokenPath = "/token"

// FCMMessage is a message the fake accepted.
type FCMMessage struct {
	Name         string // projects/<project>/messages/<n>, as returned
	Token        string
	Topic        string
	Condition    string
	Notification *FCMNotification
	Data         map[string]string
	Raw          json.RawMessage // the "message" object as sent
}

// FCMNotification is the notification part of a message.
type FCMNotification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// FCM is a fake FCM HTTP v1 API with the OAuth 2.0 token endpoint the
// client gets its access token from:
//   - POST TokenURL() with grant_type jwt-bearer returns an access token.
//     The assertion is not checked.
//   - POST SendPath() needs "Authorization: Bearer <a token it issued>",
//     else 401 UNAUTHENTICATED. A message must target exactly one of
//     token, topic and condition and carry string data values, else 400
//     INVALID_ARGUMENT. A device token set with FailToken gets that error.
//     Any other message is accepted and gets a name.
//
// Responses enqueued for SendPath() come before this default behavior.
type FCM struct {
	*Server
	project string

	mu       sync.Mutex
	issued   map[string]bool
	failures map[string]FCMErrorCode
	accepted []FCMMessage
}

// NewFCM starts a fake FCM for project; it stops at cleanup.
func NewFCM(tb testing.TB, project string) *FCM {
	tb.Helper()
	f := &FCM{project: project, issued: map[string]bool{}, failures: map[string]FCMErrorCode{}}
	f.Server = newServer(tb, "fcm", f.check, f.fallback)
	return f
}

// SendPath is the messages:send path for the fake's project.
func (f *FCM) SendPath() string { return "/v1/projects/" + f.project + "/messages:send" }

// TokenURL is the OAuth 2.0 token endpoint to configure in place of
// Google's.
func (f *FCM) TokenURL() string { return f.URL() + fcmTokenPath }

// FailToken makes every later send to deviceToken fail with code, as a
// token the app uninstalled fails with UNREGISTERED.
func (f *FCM) FailToken(deviceToken string, code FCMErrorCode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[deviceToken] = code
}

// Messages returns the messages accepted so far, in order. A send answered
// from Enqueue or refused is not among them; Requests has every request.
func (f *FCM) Messages() []FCMMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FCMMessage, len(f.accepted))
	copy(out, f.accepted)
	return out
}

// ErrorResponse is FCM's error answer for code: the Google API error with
// an FcmError detail, and the HTTP status FCM uses for it.
func (f *FCM) ErrorResponse(code FCMErrorCode) Response {
	return fcmError(code, "fake FCM error "+string(code))
}

// fcmStatus is the HTTP status and Google API status FCM answers an
// FcmError code with.
type fcmStatus struct {
	httpStatus int
	status     string
}

func fcmStatuses() map[FCMErrorCode]fcmStatus {
	return map[FCMErrorCode]fcmStatus{
		FCMUnregistered:        {http.StatusNotFound, "NOT_FOUND"},
		FCMInvalidArgument:     {http.StatusBadRequest, "INVALID_ARGUMENT"},
		FCMQuotaExceeded:       {http.StatusTooManyRequests, "RESOURCE_EXHAUSTED"},
		FCMUnavailable:         {http.StatusServiceUnavailable, "UNAVAILABLE"},
		FCMInternal:            {http.StatusInternalServerError, "INTERNAL"},
		FCMSenderIDMismatch:    {http.StatusForbidden, "PERMISSION_DENIED"},
		FCMThirdPartyAuthError: {http.StatusUnauthorized, "UNAUTHENTICATED"},
	}
}

func fcmError(code FCMErrorCode, message string) Response {
	s, ok := fcmStatuses()[code]
	if !ok {
		panic("fakes: unknown FCM error code " + string(code))
	}
	return googleError(s.httpStatus, s.status, message, []any{map[string]string{
		"@type":     "type.googleapis.com/google.firebase.fcm.v1.FcmError",
		"errorCode": string(code),
	}})
}

func googleError(httpStatus int, status, message string, details []any) Response {
	body := map[string]any{"code": httpStatus, "message": message, "status": status}
	if len(details) > 0 {
		body["details"] = details
	}
	return JSON(httpStatus, map[string]any{"error": body})
}

// check refuses a send without a token the fake issued.
func (f *FCM) check(r Request) (Response, bool) {
	if r.Path != f.SendPath() {
		return Response{}, false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	known := ok && f.issued[token]
	f.mu.Unlock()
	if known {
		return Response{}, false
	}
	return googleError(http.StatusUnauthorized, "UNAUTHENTICATED",
		"Request had invalid authentication credentials.", nil), true
}

func (f *FCM) fallback(r Request) (Response, bool) {
	switch {
	case r.Method == http.MethodPost && r.Path == fcmTokenPath:
		return f.issueToken(r), true
	case r.Method == http.MethodPost && r.Path == f.SendPath():
		return f.send(r), true
	default:
		return Response{}, false
	}
}

func (f *FCM) issueToken(r Request) Response {
	form, err := url.ParseQuery(string(r.Body))
	if err != nil || form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || form.Get("assertion") == "" {
		return JSON(http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
	}
	f.mu.Lock()
	issued := "fcm-fake-access-" + strconv.Itoa(len(f.issued)+1)
	f.issued[issued] = true
	f.mu.Unlock()
	return JSON(http.StatusOK, map[string]any{
		"access_token": issued,
		"token_type":   "Bearer",
		"expires_in":   3600,
	})
}

func (f *FCM) send(r Request) Response {
	var req struct {
		Message json.RawMessage `json:"message"`
	}
	var msg struct {
		Token        string            `json:"token"`
		Topic        string            `json:"topic"`
		Condition    string            `json:"condition"`
		Notification *FCMNotification  `json:"notification"`
		Data         map[string]string `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &req); err != nil || len(req.Message) == 0 {
		return fcmError(FCMInvalidArgument, "Request body is not a JSON object with a message.")
	}
	if err := json.Unmarshal(req.Message, &msg); err != nil {
		return fcmError(FCMInvalidArgument, "Invalid message: "+err.Error())
	}
	targets := 0
	for _, t := range []string{msg.Token, msg.Topic, msg.Condition} {
		if t != "" {
			targets++
		}
	}
	if targets != 1 {
		return fcmError(FCMInvalidArgument, "A message must have exactly one of token, topic and condition.")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if code, ok := f.failures[msg.Token]; ok && msg.Token != "" {
		return fcmError(code, "fake FCM error "+string(code))
	}
	name := "projects/" + f.project + "/messages/" + strconv.Itoa(len(f.accepted)+1)
	f.accepted = append(f.accepted, FCMMessage{
		Name:         name,
		Token:        msg.Token,
		Topic:        msg.Topic,
		Condition:    msg.Condition,
		Notification: msg.Notification,
		Data:         msg.Data,
		Raw:          req.Message,
	})
	return JSON(http.StatusOK, map[string]string{"name": name})
}
