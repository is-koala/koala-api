// Package iletimerkezi sends text messages through İleti Merkezi's REST
// API (plan §3.1 SMS row; ADR-0033): one JSON POST per message, plain
// net/http, no SDK. It is called from River jobs only, never inside a
// database transaction (CLAUDE.md "Transaction").
//
// The wire format is the provider's documented JSON API (POST
// /v1/send-sms/json): the request is wrapped in "request", the answer in
// "response", the recipients key is spelled as the provider spells it
// (RecipientsKey), and status.code is a string or a number. Java's client
// (IletiMerkeziSmsHelper) sends "recipients" and reads the answer without
// its "response" wrapper; this package follows the provider, not Java.
//
// A recipient is personal data: no error this package returns carries the
// number, the text or the provider's free-text message, and the
// credentials travel only in the request body.
package iletimerkezi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/is-koala/koala-api/internal/platform/sms"
)

// DefaultBaseURL is the provider's API origin.
const DefaultBaseURL = "https://api.iletimerkezi.com"

// SendPath is the JSON send endpoint, relative to the base URL.
const SendPath = "/v1/send-sms/json"

// timeout bounds one send; a River job's own timeout bounds the job.
const timeout = 15 * time.Second

// maxAnswer bounds the answer read from the provider.
const maxAnswer = 64 << 10

// Status codes of the provider's answer (its documentation's table).
const (
	statusOK = 200
	// statusRecipients: the recipients are missing, invalid or blocked.
	statusRecipients = 452
	// statusOrderSize: the order is over the provider's maximum size
	// (seven message parts).
	statusOrderSize = 453
	// statusEmpty: the text is empty.
	statusEmpty = 454
	// statusBadNumber: an invalid number, usually a wrong Turkish
	// carrier prefix.
	statusBadNumber = 466
)

// permanent are the answers a retry cannot change: the recipient or the
// message itself is refused. Every other refusal (credentials, balance,
// sender name, a malformed request, the provider busy) is an operational
// fault a retry may outlive once it is fixed.
func permanent(code int) bool {
	return code == statusRecipients || code == statusOrderSize || code == statusEmpty || code == statusBadNumber
}

// Config is the provider account and where its API is.
type Config struct {
	// BaseURL is DefaultBaseURL when empty; tests point it at the fake.
	BaseURL string
	// Key and Hash are the account's API credentials; Hash is a secret.
	Key  string
	Hash string
	// Sender is the approved sender name (başlık) the messages carry.
	Sender string
	// HTTPClient is a client with timeout when nil.
	HTTPClient *http.Client
}

// Client sends text messages.
type Client struct {
	endpoint string
	key      string
	hash     string
	sender   string
	http     *http.Client
}

// errRedirect refuses every redirect of the provider's answer.
var errRedirect = errors.New("iletimerkezi: the provider answered with a redirect, which is not followed")

// New checks cfg and returns the client. Nothing is sent until Send.
func New(cfg Config) (*Client, error) {
	if cfg.Key == "" || cfg.Hash == "" || cfg.Sender == "" {
		return nil, errors.New("iletimerkezi: key, hash and sender are required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("iletimerkezi: the base URL must be an absolute http or https URL")
	}
	client := &http.Client{Timeout: timeout}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		client = &c
	}
	// A redirect is refused: a 307 or 308 would carry the credentials in
	// the body to wherever it points.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	return &Client{
		endpoint: strings.TrimSuffix(u.String(), "/") + SendPath,
		key:      cfg.Key, hash: cfg.Hash, sender: cfg.Sender, http: client,
	}, nil
}

// envelope is the request body.
type envelope struct {
	Request request `json:"request"`
}

type request struct {
	Authentication authentication `json:"authentication"`
	Order          order          `json:"order"`
}

type authentication struct {
	Key  string `json:"key"`
	Hash string `json:"hash"`
}

// order is the order of one message. sendDateTime is left out: the
// message goes at once (the field is an optional "dd/MM/yyyy HH:mm"
// string; Java does not send it either).
type order struct {
	Sender string `json:"sender"`
	// IYS is "0": a verification code is not a commercial message, so
	// the İYS consent query does not apply (Java sends the same).
	IYS     string  `json:"iys"`
	IYSList string  `json:"iysList"`
	Message message `json:"message"`
}

// RecipientsKey is the provider's JSON key for the recipients, a
// transposition of "recipients" that its documentation uses. It is
// written in two parts so a spell checker cannot "correct" a wire name.
const RecipientsKey = "rece" + "ipents"

type message struct {
	Text       string
	Recipients recipients
}

// MarshalJSON writes the message with the provider's key names.
func (m message) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(map[string]any{"text": m.Text, RecipientsKey: m.Recipients})
	if err != nil {
		return nil, fmt.Errorf("iletimerkezi: encode the message: %w", err)
	}
	return b, nil
}

type recipients struct {
	Number []string `json:"number"`
}

// answer is the response body.
type answer struct {
	Response struct {
		Status struct {
			Code json.RawMessage `json:"code"`
		} `json:"status"`
	} `json:"response"`
}

// National turns an E.164 Turkish number into the ten digits the
// provider documents (5XXXXXXXXX). ok is false for any other number.
func National(e164 string) (string, bool) {
	n, ok := strings.CutPrefix(e164, "+90")
	if !ok || len(n) != 10 {
		return "", false
	}
	for _, r := range n {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return n, true
}

// Send delivers m. A recipient the provider cannot take and a refusal a
// retry cannot change wrap sms.ErrUndeliverable; any other failure is
// returned for the job to retry.
func (c *Client) Send(ctx context.Context, m sms.Message) error {
	number, ok := National(m.To)
	if !ok {
		return fmt.Errorf("iletimerkezi: the recipient is not a Turkish number: %w", sms.ErrUndeliverable)
	}
	body, err := json.Marshal(envelope{Request: request{
		Authentication: authentication{Key: c.key, Hash: c.hash},
		Order: order{
			Sender: c.sender, IYS: "0", IYSList: "",
			Message: message{Text: m.Text, Recipients: recipients{Number: []string{number}}},
		},
	}})
	if err != nil {
		return fmt.Errorf("iletimerkezi: encode the request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("iletimerkezi: build the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("iletimerkezi: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return fmt.Errorf("iletimerkezi: read the answer (HTTP %d): %w", resp.StatusCode, err)
	}
	code, ok := statusOf(raw)
	switch {
	case !ok:
		return fmt.Errorf("iletimerkezi: unreadable answer (HTTP %d)", resp.StatusCode)
	case code == statusOK:
		return nil
	case permanent(code):
		return fmt.Errorf("iletimerkezi: refused with status %d: %w", code, sms.ErrUndeliverable)
	default:
		return fmt.Errorf("iletimerkezi: refused with status %d (HTTP %d)", code, resp.StatusCode)
	}
}

// statusOf reads response.status.code, a string or a number.
func statusOf(raw []byte) (int, bool) {
	var a answer
	if err := json.Unmarshal(raw, &a); err != nil || len(a.Response.Status.Code) == 0 {
		return 0, false
	}
	s := strings.Trim(string(a.Response.Status.Code), `"`)
	code, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return code, true
}
