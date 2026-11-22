package fakes

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
)

// The İleti Merkezi send endpoint and the status codes of its answers
// (the provider's documentation).
const (
	iletiMerkeziSendPath = "/v1/send-sms/json"

	IletiMerkeziOK          = 200
	IletiMerkeziMalformed   = 400
	IletiMerkeziBadAuth     = 401
	IletiMerkeziNoBalance   = 402
	IletiMerkeziBadSender   = 450
	IletiMerkeziRecipients  = 452
	IletiMerkeziTooLong     = 453
	IletiMerkeziEmptyText   = 454
	iletiMerkeziRecipientsK = "rece" + "ipents" // the provider's spelling of the key
)

// IletiMerkeziMessage is a message the fake accepted.
type IletiMerkeziMessage struct {
	Sender  string
	IYS     string
	Text    string
	Numbers []string
	OrderID string
}

// IletiMerkezi is a fake of İleti Merkezi's JSON SMS API:
//   - POST SendPath() takes {"request": {"authentication": {key, hash},
//     "order": {sender, iys, message: {text, <recipients>: {number: [...]}}}}}
//     and answers {"response": {"status": {code, message}, "order": {id}}}
//     with HTTP 200, as the provider does; the outcome is in the code.
//   - Credentials other than the fake's get 401. A request it cannot read,
//     or without a sender, text or number, gets 400. A number set with
//     RejectNumber gets 452. Anything else is accepted and gets an order
//     id; its code is the string "200", as in the documentation.
//
// Responses enqueued for SendPath() come after the credential check and
// before this default behavior.
type IletiMerkezi struct {
	*Server
	key, hash string

	mu       sync.Mutex
	rejected map[string]bool
	accepted []IletiMerkeziMessage
}

// NewIletiMerkezi starts a fake that accepts the credentials key and hash;
// it stops at cleanup.
func NewIletiMerkezi(tb testing.TB, key, hash string) *IletiMerkezi {
	tb.Helper()
	f := &IletiMerkezi{key: key, hash: hash, rejected: map[string]bool{}}
	f.Server = newServer(tb, "iletimerkezi", f.check, f.fallback)
	return f
}

// SendPath is the JSON send endpoint.
func (f *IletiMerkezi) SendPath() string { return iletiMerkeziSendPath }

// RejectNumber makes every later send to number (as sent: ten digits)
// fail with 452, as for a blocked or invalid number.
func (f *IletiMerkezi) RejectNumber(number string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected[number] = true
}

// Messages returns the messages accepted so far, in order.
func (f *IletiMerkezi) Messages() []IletiMerkeziMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]IletiMerkeziMessage, len(f.accepted))
	copy(out, f.accepted)
	return out
}

// IletiMerkeziStatus is the provider's answer with code. numeric writes
// the code as a JSON number instead of a string: the documentation shows
// both.
func IletiMerkeziStatus(code int, numeric bool) Response {
	var c any = strconv.Itoa(code)
	if numeric {
		c = code
	}
	return JSON(http.StatusOK, map[string]any{"response": map[string]any{
		"status": map[string]any{"code": c, "message": "fake status " + strconv.Itoa(code)},
	}})
}

// iletiMerkeziRequest is the request body as the fake reads it.
type iletiMerkeziRequest struct {
	Request struct {
		Authentication struct {
			Key  string `json:"key"`
			Hash string `json:"hash"`
		} `json:"authentication"`
		Order struct {
			Sender  string                     `json:"sender"`
			IYS     string                     `json:"iys"`
			Message map[string]json.RawMessage `json:"message"`
		} `json:"order"`
	} `json:"request"`
}

// check refuses foreign credentials before anything else.
func (f *IletiMerkezi) check(r Request) (Response, bool) {
	if r.Path != iletiMerkeziSendPath {
		return Response{}, false
	}
	var req iletiMerkeziRequest
	if err := json.Unmarshal(r.Body, &req); err != nil {
		return IletiMerkeziStatus(IletiMerkeziMalformed, false), true
	}
	if req.Request.Authentication.Key != f.key || req.Request.Authentication.Hash != f.hash {
		return IletiMerkeziStatus(IletiMerkeziBadAuth, false), true
	}
	return Response{}, false
}

func (f *IletiMerkezi) fallback(r Request) (Response, bool) {
	if r.Method != http.MethodPost || r.Path != iletiMerkeziSendPath {
		return Response{}, false
	}
	var req iletiMerkeziRequest
	if err := json.Unmarshal(r.Body, &req); err != nil {
		return IletiMerkeziStatus(IletiMerkeziMalformed, false), true
	}
	var text string
	var to struct {
		Number []string `json:"number"`
	}
	order := req.Request.Order
	if err := json.Unmarshal(order.Message["text"], &text); err != nil || text == "" {
		return IletiMerkeziStatus(IletiMerkeziEmptyText, false), true
	}
	if err := json.Unmarshal(order.Message[iletiMerkeziRecipientsK], &to); err != nil || len(to.Number) == 0 {
		return IletiMerkeziStatus(IletiMerkeziRecipients, false), true
	}
	if order.Sender == "" {
		return IletiMerkeziStatus(IletiMerkeziBadSender, false), true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range to.Number {
		if f.rejected[n] {
			return IletiMerkeziStatus(IletiMerkeziRecipients, false), true
		}
	}
	id := strconv.Itoa(1000 + len(f.accepted))
	f.accepted = append(f.accepted, IletiMerkeziMessage{
		Sender: order.Sender, IYS: order.IYS, Text: text, Numbers: to.Number, OrderID: id,
	})
	return JSON(http.StatusOK, map[string]any{"response": map[string]any{
		"status": map[string]any{"code": "200", "message": "İşlem başarılı"},
		"order":  map[string]any{"id": id},
	}}), true
}
