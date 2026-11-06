package audit

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MaskBody is the JSON a detail row stores for body, with its personal
// data masked at any depth:
//   - by key: a member whose key contains a personal part (piiKeyParts)
//     or ends in a personal suffix (piiKeySuffixes) keeps its key and
//     gets Masked as value, whatever the value is;
//   - by value: a string that contains an e-mail address, is a phone
//     number (10 to 15 digits once spaces, dashes, dots and parentheses
//     are dropped) or is a payment card number (13 to 19 digits that pass
//     the Luhn checksum) is Masked wherever it appears, whatever its key.
//     A JSON number is read the same way, so a phone or a card sent
//     unquoted is masked too.
//
// The two layers are a deny list: free text and a personal value under an
// unlisted key go through. A move to an allow list (nothing but the keys
// an operation declares) is phase 2 work.
//
// A result over MaxBodyBytes is replaced by {"truncated": true, "bytes": n}.
func MaskBody(body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("audit: encode body: %w", err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("audit: decode body: %w", err)
	}
	out, err := json.Marshal(mask(v))
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
)

// numberDigits drops the separators people write in phone and card
// numbers.
func numberDigits(s string) string {
	return strings.NewReplacer(" ", "", "-", "", ".", "", "(", "", ")", "").Replace(s)
}

// personalNumber reports whether s is a phone number or a payment card
// number. A 10 to 15 digit run is a phone number, so an epoch in
// milliseconds is masked with it: masking a timestamp costs a reader of
// the trail little, printing a phone number costs the data subject a lot.
// Card numbers are longer and only count when they pass Luhn, which keeps
// ids and counters out.
func personalNumber(s string) bool {
	d := numberDigits(s)
	return phoneValue.MatchString(d) || (cardValue.MatchString(d) && luhn(d))
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

func mask(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, member := range t {
			if personal(k) {
				t[k] = Masked
				continue
			}
			t[k] = mask(member)
		}
		return t
	case []any:
		for i, member := range t {
			t[i] = mask(member)
		}
		return t
	case string:
		if emailInValue.MatchString(t) || personalNumber(t) {
			return Masked
		}
		return t
	case float64: // every JSON number decodes into any as a float64
		if personalNumber(strconv.FormatFloat(t, 'f', -1, 64)) {
			return Masked
		}
		return t
	default:
		return v
	}
}

// personal reports whether a member's key makes its value personal data
// or a secret. The rule and its exceptions are both read as endings, so a
// composed key is judged like the bare one it ends in: "targetRoleName"
// is exempt like "roleName", and "mustChangePassword" like itself.
func personal(key string) bool {
	k := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
	if slices.ContainsFunc(piiKeyExceptions(), func(e string) bool { return strings.HasSuffix(k, e) }) {
		return false
	}
	return slices.ContainsFunc(piiKeyParts(), func(p string) bool { return strings.Contains(k, p) }) ||
		slices.ContainsFunc(piiKeySuffixes(), func(p string) bool { return strings.HasSuffix(k, p) })
}
