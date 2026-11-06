package audit

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// MaskBody is the JSON a detail row stores for body, with its personal
// data masked at any depth:
//   - by key: a member whose key contains a personal part (piiKeyParts)
//     or ends in a personal suffix (piiKeySuffixes) keeps its key and
//     gets Masked as value, whatever the value is;
//   - by value: a string that contains an e-mail address or is a phone
//     number (10 to 15 digits once spaces, dashes, dots and parentheses
//     are dropped) is Masked wherever it appears, whatever its key.
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
)

// phoneDigits drops the separators people write in phone numbers.
func phoneDigits(s string) string {
	return strings.NewReplacer(" ", "", "-", "", ".", "", "(", "", ")", "").Replace(s)
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
		if emailInValue.MatchString(t) || phoneValue.MatchString(phoneDigits(t)) {
			return Masked
		}
		return t
	default:
		return v
	}
}

func personal(key string) bool {
	k := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
	if slices.Contains(piiKeyExceptions(), k) {
		return false
	}
	return slices.ContainsFunc(piiKeyParts(), func(p string) bool { return strings.Contains(k, p) }) ||
		slices.ContainsFunc(piiKeySuffixes(), func(p string) bool { return strings.HasSuffix(k, p) })
}
