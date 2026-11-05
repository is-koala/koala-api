package fakes

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Iyzico endpoints the Java backend calls (koala-backend
// IyzicoPaymentService). The Faz 6 client calls the same paths.
const (
	IyzicoPathCheckoutFormInitialize = "/payment/iyzipos/checkoutform/initialize/auth/ecom"
	IyzicoPathCheckoutFormRetrieve   = "/payment/iyzipos/checkoutform/auth/ecom/detail"
	IyzicoPathApprove                = "/payment/iyzipos/item/approve"
	IyzicoPathDisapprove             = "/payment/iyzipos/item/disapprove"
	IyzicoPathRefund                 = "/payment/refund"
	IyzicoPathCancel                 = "/payment/cancel"
	IyzicoPathSubmerchant            = "/onboarding/submerchant"
)

// IyzicoKeys are the credentials the fake expects and signs with. Tests use
// the constants of testdata/iyzico/vectors.json, never real keys.
type IyzicoKeys struct {
	APIKey    string
	SecretKey string
}

// Iyzico is a fake Iyzico API (IYZWSv2). It authenticates every request
// like Iyzico does: x-iyzi-rnd and an IYZWSv2 Authorization whose
// signature is hex(HMAC-SHA256(secret, randomKey + path + body)). A request
// that fails is answered 401 and consumes no programmed response; the body
// of that 401 is the fake's own, Iyzico's real error body is not modeled.
//
// It has no default answers: a test programs each one with Enqueue, using
// the builders below for signed responses.
type Iyzico struct {
	*Server
	keys IyzicoKeys
}

// NewIyzico starts a fake Iyzico for the test; it stops at cleanup.
func NewIyzico(tb testing.TB, keys IyzicoKeys) *Iyzico {
	tb.Helper()
	f := &Iyzico{keys: keys}
	f.Server = newServer(tb, "iyzico", f.check, nil)
	return f
}

func (f *Iyzico) check(r Request) (Response, bool) {
	if err := f.VerifyRequest(r); err != nil {
		return JSON(http.StatusUnauthorized, map[string]string{
			"status":       "failure",
			"errorCode":    "FAKE_UNAUTHORIZED",
			"errorMessage": err.Error(),
			"locale":       "tr",
		}), true
	}
	return Response{}, false
}

// VerifyRequest checks r's IYZWSv2 authentication against the fake's keys.
func (f *Iyzico) VerifyRequest(r Request) error {
	randomKey := r.Header.Get("X-Iyzi-Rnd")
	if randomKey == "" {
		return errors.New("iyzico: no x-iyzi-rnd header")
	}
	encoded, ok := strings.CutPrefix(r.Header.Get("Authorization"), "IYZWSv2 ")
	if !ok {
		return errors.New("iyzico: Authorization is not IYZWSv2")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("iyzico: Authorization is not base64: %w", err)
	}
	parts := strings.Split(string(raw), "&")
	if len(parts) != 3 {
		return errors.New("iyzico: Authorization must be apiKey:…&randomKey:…&signature:…")
	}
	apiKey, ok1 := strings.CutPrefix(parts[0], "apiKey:")
	authRandomKey, ok2 := strings.CutPrefix(parts[1], "randomKey:")
	signature, ok3 := strings.CutPrefix(parts[2], "signature:")
	switch {
	case !ok1 || !ok2 || !ok3:
		return errors.New("iyzico: Authorization must be apiKey:…&randomKey:…&signature:…")
	case apiKey != f.keys.APIKey:
		return errors.New("iyzico: unknown api key")
	case authRandomKey != randomKey:
		return errors.New("iyzico: randomKey differs from x-iyzi-rnd")
	}
	want := iyzicoRequestSignature(f.keys.SecretKey, randomKey, r.Path, r.Body)
	if !hmac.Equal([]byte(strings.ToLower(signature)), []byte(want)) {
		return errors.New("iyzico: signature mismatch")
	}
	return nil
}

// Sign returns the response signature over fields as Iyzico computes it:
// hex(HMAC-SHA256(secret, fields joined with ':')).
func (f *Iyzico) Sign(fields ...string) string {
	return hmacHex(f.keys.SecretKey, strings.Join(fields, ":"))
}

// CheckoutFormInitialized is a successful, signed checkout form initialize
// response; the signature covers conversationId and token.
func (f *Iyzico) CheckoutFormInitialized(conversationID, token, paymentPageURL string) Response {
	return JSON(http.StatusOK, map[string]any{
		"status":         "success",
		"locale":         "tr",
		"conversationId": conversationID,
		"token":          token,
		"paymentPageUrl": paymentPageURL,
		"signature":      f.Sign(conversationID, token),
	})
}

// CheckoutForm is the result of a checkout form retrieve. PaidPrice and
// Price are decimals as Iyzico writes them ("1075.00").
type CheckoutForm struct {
	PaymentStatus  string
	PaymentID      string
	Currency       string
	BasketID       string
	ConversationID string
	PaidPrice      string
	Price          string
	Token          string
}

// CheckoutFormRetrieved is a successful, signed checkout form retrieve
// response. The prices go into the body as JSON numbers with the given
// text and into the signature without trailing zeros (Java's
// BigDecimal.stripTrailingZeros().toPlainString()). It panics on a price
// that is not a decimal, which is a mistake in the test.
func (f *Iyzico) CheckoutFormRetrieved(c CheckoutForm) Response {
	paid, err := iyzicoDecimal(c.PaidPrice)
	if err != nil {
		panic(fmt.Sprintf("fakes: paidPrice: %v", err))
	}
	price, err := iyzicoDecimal(c.Price)
	if err != nil {
		panic(fmt.Sprintf("fakes: price: %v", err))
	}
	return JSON(http.StatusOK, map[string]any{
		"status":         "success",
		"locale":         "tr",
		"paymentStatus":  c.PaymentStatus,
		"paymentId":      c.PaymentID,
		"currency":       c.Currency,
		"basketId":       c.BasketID,
		"conversationId": c.ConversationID,
		"paidPrice":      json.Number(c.PaidPrice),
		"price":          json.Number(c.Price),
		"token":          c.Token,
		"signature": f.Sign(c.PaymentStatus, c.PaymentID, c.Currency, c.BasketID,
			c.ConversationID, paid, price, c.Token),
	})
}

// Success is an unsigned success response with fields added: approve,
// disapprove, refund and sub-merchant responses carry no signature the
// client checks.
func (f *Iyzico) Success(fields map[string]any) Response {
	body := map[string]any{"status": "success", "locale": "tr"}
	for k, v := range fields {
		body[k] = v
	}
	return JSON(http.StatusOK, body)
}

// Failure is an Iyzico business failure: HTTP 200 with status "failure".
func (f *Iyzico) Failure(errorCode, errorMessage string) Response {
	return JSON(http.StatusOK, map[string]any{
		"status":       "failure",
		"locale":       "tr",
		"errorCode":    errorCode,
		"errorMessage": errorMessage,
	})
}

// iyzicoRequestSignature is the IYZWSv2 request signature: the payload is
// the URI path and the body (only the path when the body is empty), no
// query.
func iyzicoRequestSignature(secretKey, randomKey, path string, body []byte) string {
	return hmacHex(secretKey, randomKey+path+string(body))
}

func hmacHex(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	_, _ = m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// iyzicoDecimal writes decimal s as Java's
// new BigDecimal(s).stripTrailingZeros().toPlainString() does for the plain
// decimals Iyzico sends: no exponent, no trailing fraction zeros, no
// leading zeros, "0" for zero.
func iyzicoDecimal(s string) (string, error) {
	digits, negative := strings.CutPrefix(s, "-")
	whole, frac, hasPoint := strings.Cut(digits, ".")
	if whole == "" || !isDigits(whole) || !isDigits(frac) || (hasPoint && frac == "") {
		return "", fmt.Errorf("%q is not a plain decimal", s)
	}
	frac = strings.TrimRight(frac, "0")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	if whole == "0" && frac == "" {
		return "0", nil
	}
	out := whole
	if frac != "" {
		out += "." + frac
	}
	if negative {
		out = "-" + out
	}
	return out, nil
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
