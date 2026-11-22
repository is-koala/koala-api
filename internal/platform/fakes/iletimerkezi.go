package fakes

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
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
	IletiMerkeziOrderSize   = 453
	IletiMerkeziEmptyText   = 454
	IletiMerkeziBadDate     = 457
	IletiMerkeziNoSender    = 459
	IletiMerkeziSenderForm  = 460
	IletiMerkeziBadNumber   = 466
	IletiMerkeziNoIYS       = 468
	iletiMerkeziRecipientsK = "rece" + "ipents" // the provider's spelling of the key

	// iletiMerkeziMaxParts is the order size limit: seven message parts,
	// 153 characters a part in the GSM alphabet and 148 with Turkish
	// letters (the provider's documentation).
	iletiMerkeziMaxParts = 7
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
//     with the code as the HTTP status too, as the provider does.
//   - Credentials other than the fake's get 401. A request it cannot read
//     gets 400. As the provider documents: no iys flag 468, a sendDateTime
//     that is not a "dd/MM/yyyy HH:mm" string 457, no sender 459, a sender
//     over 11 characters or with Turkish letters 460, a sender other than
//     the approved one 450, an empty text 454, a text over seven parts
//     453, a missing recipients member (the correct spelling of the key
//     included), no number or a number that is not 905XXXXXXXXX,
//     +905XXXXXXXXX or 5XXXXXXXXX 452. A number set with RejectNumber gets
//     452 too; 466 (an invalid carrier prefix) is the blacklist API's,
//     and IletiMerkeziBadNumber lets a test enqueue it. Anything else is
//     accepted and gets an order id; its code is the string "200".
//
// Responses enqueued for SendPath() come after the credential check and
// before this default behavior.
type IletiMerkezi struct {
	*Server
	key, hash, sender string

	mu       sync.Mutex
	rejected map[string]bool
	accepted []IletiMerkeziMessage
}

// NewIletiMerkezi starts a fake that accepts the credentials key and hash
// and the approved sender name sender; it stops at cleanup.
func NewIletiMerkezi(tb testing.TB, key, hash, sender string) *IletiMerkezi {
	tb.Helper()
	f := &IletiMerkezi{key: key, hash: hash, sender: sender, rejected: map[string]bool{}}
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
	return JSON(code, map[string]any{"response": map[string]any{
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
			Sender       string                     `json:"sender"`
			IYS          *string                    `json:"iys"`
			SendDateTime json.RawMessage            `json:"sendDateTime"`
			Message      map[string]json.RawMessage `json:"message"`
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
	order := req.Request.Order
	if code := iletiMerkeziOrderCode(order.IYS, order.SendDateTime, order.Sender, f.sender); code != 0 {
		return IletiMerkeziStatus(code, false), true
	}
	var text string
	if err := json.Unmarshal(order.Message["text"], &text); err != nil || text == "" {
		return IletiMerkeziStatus(IletiMerkeziEmptyText, false), true
	}
	if iletiMerkeziParts(text) > iletiMerkeziMaxParts {
		return IletiMerkeziStatus(IletiMerkeziOrderSize, false), true
	}
	var to struct {
		Number []string `json:"number"`
	}
	if err := json.Unmarshal(order.Message[iletiMerkeziRecipientsK], &to); err != nil || len(to.Number) == 0 {
		return IletiMerkeziStatus(IletiMerkeziRecipients, false), true
	}
	for _, n := range to.Number {
		if !iletiMerkeziNumber.MatchString(n) {
			return IletiMerkeziStatus(IletiMerkeziRecipients, false), true
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range to.Number {
		if f.rejected[n] {
			return IletiMerkeziStatus(IletiMerkeziRecipients, false), true
		}
	}
	iys := ""
	if order.IYS != nil {
		iys = *order.IYS
	}
	id := strconv.Itoa(1000 + len(f.accepted))
	f.accepted = append(f.accepted, IletiMerkeziMessage{
		Sender: order.Sender, IYS: iys, Text: text, Numbers: to.Number, OrderID: id,
	})
	return JSON(http.StatusOK, map[string]any{"response": map[string]any{
		"status": map[string]any{"code": "200", "message": "İşlem başarılı"},
		"order":  map[string]any{"id": id},
	}}), true
}

// iletiMerkeziNumber is a number form the provider accepts.
var iletiMerkeziNumber = regexp.MustCompile(`^(\+?90)?5\d{9}$`)

// iletiMerkeziDate is the provider's sendDateTime form.
var iletiMerkeziDate = regexp.MustCompile(`^\d{2}/\d{2}/\d{4} \d{2}:\d{2}$`)

// iletiMerkeziOrderCode checks an order's flags and sender in the
// provider's order; 0 when they pass.
func iletiMerkeziOrderCode(iys *string, sendDateTime json.RawMessage, sender, approved string) int {
	if iys == nil || (*iys != "0" && *iys != "1") {
		return IletiMerkeziNoIYS
	}
	if len(sendDateTime) > 0 {
		var when string
		if err := json.Unmarshal(sendDateTime, &when); err != nil || !iletiMerkeziDate.MatchString(when) {
			return IletiMerkeziBadDate
		}
	}
	switch {
	case sender == "":
		return IletiMerkeziNoSender
	case utf8.RuneCountInString(sender) > 11 || strings.ContainsAny(sender, "çğıöşüÇĞİÖŞÜ"):
		return IletiMerkeziSenderForm
	case sender != approved:
		return IletiMerkeziBadSender
	}
	return 0
}

// iletiMerkeziParts is how many message parts text takes: 153 characters
// a part in the GSM alphabet, 148 once a Turkish letter is in it.
func iletiMerkeziParts(text string) int {
	per := 153
	if strings.ContainsAny(text, "çğıöşüÇĞİÖŞÜ") {
		per = 148
	}
	n := utf8.RuneCountInString(text)
	return (n + per - 1) / per
}
