package audit

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// MaskBody is the JSON a detail row stores for body: every member whose key
// names personal data or a secret (piiKeys) keeps its key and gets Masked
// as value, at any depth; a result over MaxBodyBytes is replaced by
// {"truncated": true, "bytes": n}. The masking is by key, the way log
// masking works: a body member is personal data by what it is called,
// whatever its value looks like.
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
	default:
		return v
	}
}

func personal(key string) bool {
	k := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
	return slices.Contains(piiKeys(), k)
}
