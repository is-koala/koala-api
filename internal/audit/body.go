package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MaskBody is the JSON a detail row stores for body: its shape, and of
// its values only the ones an operation is allowed to put in the trail
// (ADR-0028). Two layers, in this order:
//
//   - by key, an allow list: a scalar stays in clear only when the key
//     naming it is in allowedKeys or matches a class in
//     allowedKeyClasses (which is itself narrowed by piiKeyParts).
//     Everything else becomes Masked, whatever it holds - free text, a
//     personal field nobody listed, a key invented tomorrow. Objects and
//     arrays are walked instead of masked, so the body keeps its keys,
//     its nesting and the length of each array, and every scalar leaf is
//     judged by the key that names it (an array's elements by the
//     array's key). A key that is itself personal data, as when a map is
//     keyed by e-mail address, is masked as a key.
//   - by value, a last defense on what the allow list let through: a
//     string that contains an e-mail address, is a phone number (10 to
//     15 digits once spaces, dashes, dots and parentheses are dropped)
//     or is a payment card number (13 to 19 digits that pass the Luhn
//     checksum) becomes Masked under any key. A JSON number is read the
//     same way, digit for digit, so a phone or a card sent unquoted is
//     masked too.
//
// The first layer is what a deny list could not be: complete. The second
// catches the case the first cannot see - a personal value put under a
// listed key, for example a card number in "doorCode" or "orderCode".
//
// A result over MaxBodyBytes is replaced by {"truncated": true, "bytes": n}.
func MaskBody(body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("audit: encode body: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Keep the digits the writer sent. json.Unmarshal decodes every
	// number into a float64, which holds 15 to 17 significant decimal
	// digits: a 17 to 19 digit card number comes back rounded, its last
	// digits changed, and the Luhn check below would miss it - the number
	// would reach the trail all but in clear.
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("audit: decode body: %w", err)
	}
	out, err := json.Marshal(mask("", v))
	if err != nil {
		return nil, fmt.Errorf("audit: encode masked body: %w", err)
	}
	if len(out) > MaxBodyBytes {
		out, err = json.Marshal(map[string]any{"truncated": true, "bytes": len(out)})
		if err != nil {
			return nil, fmt.Errorf("audit: encode truncated body: %w", err)
		}
	}
	return out, nil
}

var (
	emailInValue = regexp.MustCompile(`[^\s@"]+@[^\s@"]+\.[^\s@"]+`)
	phoneValue   = regexp.MustCompile(`^\+?\d{10,15}$`)
	cardValue    = regexp.MustCompile(`^\d{13,19}$`)
	// ibanValue is the shape of ISO 13616: two letters for the country,
	// two check digits, then 11 to 30 alphanumerics (26 for TR). An IBAN
	// is neither a phone nor a card number - it has letters and no Luhn
	// checksum - so without this it printed in clear under any allowed
	// key. The shape alone is not enough: it also matches a hex
	// identifier that happens to start with two letters and two digits,
	// which is 5.5% of the dash-stripped uuids, so iban97 below decides.
	ibanValue = regexp.MustCompile(`^[A-Z]{2}\d{2}[A-Z0-9]{11,30}$`)
)

// numberDigits drops the separators people write in phone and card
// numbers.
func numberDigits(s string) string {
	return strings.NewReplacer(" ", "", "-", "", ".", "", "(", "", ")", "").Replace(s)
}

// personalNumber reports whether the string s is a phone number or a
// payment card number, separators and all. A 10 to 15 digit run is a
// phone number, so an epoch in milliseconds is masked with it: masking a
// timestamp costs a reader of the trail little, printing a phone number
// costs the data subject a lot. Card numbers are longer and only count
// when they pass Luhn, which keeps ids and counters out.
func personalNumber(s string) bool {
	d := numberDigits(s)
	return phoneValue.MatchString(d) || (cardValue.MatchString(d) && luhn(d))
}

// personalNumberLiteral is personalNumber for a JSON number literal. Only
// an integer literal can be a phone or a card number, and the separators
// numberDigits drops are digits of neither: in "12345678901.23" the dot
// does not join two halves of one number.
func personalNumberLiteral(s string) bool {
	if strings.ContainsAny(s, ".eE+-") {
		return false
	}
	return phoneValue.MatchString(s) || (cardValue.MatchString(s) && luhn(s))
}

// luhn reports whether the digits satisfy the Luhn checksum (ISO/IEC
// 7812), which every payment card number does.
func luhn(digits string) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// mask walks v, which key names, and returns what the trail may hold:
// containers walked, scalars kept only under an allowed key and only
// when they are no personal value themselves.
func mask(key string, v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, member := range t {
			// A masked key collapses with another masked key; the member
			// values are masked either way, so only the count is lost.
			out[maskKey(k)] = mask(k, member)
		}
		return out
	case []any:
		for i, member := range t {
			t[i] = mask(key, member)
		}
		return t
	default:
		if !allowed(key) {
			return Masked
		}
		return maskValue(v)
	}
}

// maskKey masks a member's key when the key is itself personal data, as
// in a map keyed by e-mail address.
func maskKey(key string) string {
	if personalValue(key) {
		return Masked
	}
	return key
}

// maskValue is the value layer on a scalar whose key the allow list let
// through.
func maskValue(v any) any {
	switch t := v.(type) {
	case string:
		if personalValue(t) {
			return Masked
		}
		return t
	case json.Number: // UseNumber above: the digits are the writer's own
		if personalNumberLiteral(t.String()) {
			return Masked
		}
		return t
	case float64:
		// Unreachable while MaskBody decodes with UseNumber, and kept so
		// that dropping UseNumber cannot let a card number through: a
		// rounded number fails Luhn, so here the whole card length range
		// is masked without it.
		s := strconv.FormatFloat(t, 'f', -1, 64)
		if phoneValue.MatchString(s) || cardValue.MatchString(s) {
			return Masked
		}
		return t
	default: // bool, nil
		return v
	}
}

// personalValue reports whether the string is an e-mail address, a phone
// number, a payment card number or a bank account number.
func personalValue(s string) bool {
	return emailInValue.MatchString(s) || personalNumber(s) || bankAccount(s)
}

// bankAccount reports whether s is an IBAN: the shape above and the
// check digits. Spaces, dashes and dots are dropped first, so the
// groupings people write it in (TR33 0006 …, TR33-0006-…) are read as the
// one number they are. Dropping dashes makes a uuid 32 hex characters
// long, and one in twenty of those starts like an IBAN - which is why
// the checksum decides and the shape does not: with it, 0.06% of such
// uuids are mistaken for an account number instead of 5.5%.
func bankAccount(s string) bool {
	c := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", ".", "").Replace(s))
	return ibanValue.MatchString(c) && iban97(c)
}

// iban97 is the ISO 7064 MOD 97-10 check of ISO 13616: move the first
// four characters to the end, read each letter as its position plus ten
// (A=10 … Z=35), and the number read in base ten must leave 1 when
// divided by 97. The remainder is carried along digit by digit, so no
// big integer is needed.
func iban97(s string) bool {
	rotated := s[4:] + s[:4]
	rem := 0
	for i := range len(rotated) {
		switch c := rotated[i]; {
		case c >= '0' && c <= '9':
			rem = rem*10 + int(c-'0')
		case c >= 'A' && c <= 'Z':
			rem = rem*100 + int(c-'A') + 10
		default:
			return false
		}
		rem %= 97
	}
	return rem == 1
}

// normalizeKey is the form the three key lists are written in: lower
// case, without the separators a writer may put between words. Space and
// dot are dropped with "_" and "-", because a body is not only written by
// our own panel - MaskBody takes any JSON value, including a decoded
// request map - and "national id" or "user.email" must be read as the one
// key they name, not slip past the lists as something they are not.
func normalizeKey(key string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "", ".", "").Replace(strings.ToLower(key))
}

// allowed reports whether a member's key lets its value stay in the
// trail in clear. The decision is total and has one order: an exact
// allowed key, else an allowed class whose key holds no personal part,
// else masked. Nothing short-circuits, so no composition of a listed
// ending can buy an exemption the bare key does not have.
func allowed(key string) bool {
	k := normalizeKey(key)
	if slices.Contains(allowedKeys(), k) {
		return true
	}
	if !slices.ContainsFunc(allowedKeyClasses(), func(c string) bool { return strings.HasSuffix(k, c) }) {
		return false
	}
	return !slices.ContainsFunc(piiKeyParts(), func(p string) bool { return strings.Contains(k, p) })
}
