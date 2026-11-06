package audit_test

import (
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/audit"
)

// The detail body keeps its shape and loses its personal data: members
// named for personal data or secrets are masked at any depth (ADR-0026).
func TestMaskBodyMasksPersonalMembers(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"email":       "ayse@example.com",
		"roles":       []string{"OWNER"},
		"profile":     map[string]any{"Phone_Number": "+905551112233", "city": "İstanbul"},
		"items":       []any{map[string]any{"iban": "TR330006100519786457841326", "amount": 15000}},
		"newPassword": "uzun bir parola",
	}
	got, err := audit.MaskBody(body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, leaked := range []string{"ayse@example.com", "+905551112233", "TR330006100519786457841326", "uzun bir parola"} {
		if strings.Contains(s, leaked) {
			t.Errorf("masked body %s still carries %q", s, leaked)
		}
	}
	for _, kept := range []string{`"roles":["OWNER"]`, `"city":"İstanbul"`, `"amount":15000`, `"email":"***"`} {
		if !strings.Contains(s, kept) {
			t.Errorf("masked body %s lost %s", s, kept)
		}
	}
}

// Keys that contain or end in a personal part are masked, and so are
// values that are an e-mail address or a phone number under any key;
// ids, codes and names of things stay.
func TestMaskBodyBySubstringSuffixAndValue(t *testing.T) {
	t.Parallel()
	id := "9a1c0f10-0003-4c00-8000-000000000001"
	body := map[string]any{
		"contactEmail": "x",
		"user_phone":   "y",
		"ibanNumber":   "z",
		"refreshToken": "t",
		"userName":     "ayse",
		"pickupLat":    41.0,
		"note":         "beni ayse@example.com adresinden ara",
		"comment":      "0 (555) 111 22 33",
		"roleName":     "OWNER",
		"membership":   "ACTIVE",
		"targetId":     id,
		"count":        12,
	}
	got, err := audit.MaskBody(body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, k := range []string{"contactEmail", "user_phone", "ibanNumber", "refreshToken", "userName", "pickupLat", "note", "comment"} {
		if !strings.Contains(s, `"`+k+`":"***"`) {
			t.Errorf("masked body %s: %s is not masked", s, k)
		}
	}
	for _, kept := range []string{`"roleName":"OWNER"`, `"membership":"ACTIVE"`, `"targetId":"` + id + `"`, `"count":12`} {
		if !strings.Contains(s, kept) {
			t.Errorf("masked body %s lost %s", s, kept)
		}
	}
}

// A body over 8 KB after masking is replaced by a marker with its size.
func TestMaskBodyCapsTheSize(t *testing.T) {
	t.Parallel()
	got, err := audit.MaskBody(map[string]any{"note": strings.Repeat("x", audit.MaxBodyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > audit.MaxBodyBytes || !strings.Contains(string(got), `"truncated":true`) {
		t.Fatalf("capped body = %.80s…, want the truncation marker", got)
	}
}
