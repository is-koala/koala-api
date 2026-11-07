package audit_test

import (
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/audit"
)

// The detail body keeps the values an operation is allowed to record and
// loses everything else, at any depth (ADR-0028). The body below is the
// one the repository writes today (`koala staff create`) plus members an
// admin body may carry.
func TestMaskBodyKeepsAllowedValuesAndMasksTheRest(t *testing.T) {
	t.Parallel()
	id := "9a1c0f10-0003-4c00-8000-000000000001"
	body := map[string]any{
		"email":       "ayse@example.com",
		"roles":       []string{"OWNER"},
		"targetId":    id,
		"decision":    "APPROVED",
		"count":       12,
		"profile":     map[string]any{"Phone_Number": "+905551112233", "city": "İstanbul"},
		"items":       []any{map[string]any{"iban": "TR330006100519786457841326", "amount": 15000}},
		"newPassword": "uzun bir parola",
	}
	got, err := audit.MaskBody(body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, leaked := range []string{"ayse@example.com", "+905551112233", "TR330006100519786457841326", "uzun bir parola", "İstanbul"} {
		if strings.Contains(s, leaked) {
			t.Errorf("masked body %s still carries %q", s, leaked)
		}
	}
	for _, kept := range []string{
		`"roles":["OWNER"]`, `"targetId":"` + id + `"`, `"decision":"APPROVED"`, `"count":12`,
		`"amount":15000`, `"email":"***"`, `"newPassword":"***"`,
	} {
		if !strings.Contains(s, kept) {
			t.Errorf("masked body %s lost %s", s, kept)
		}
	}
}

// The allow list is what the old deny list could not be: complete. A key
// nobody listed is masked whatever it holds - free text, a name, a place,
// a value that matches no pattern at all - so a personal field Faz 2
// brings cannot reach the trail by being new.
func TestMaskBodyMasksEveryUnlistedKey(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"userName":     "ayse",
		"nickname":     "aysem",
		"city":         "Bursa",
		"note":         "beni ara",
		"reason":       "belgeler eksik",
		"pickupLat":    41.0,
		"title":        "Garson aranıyor",
		"doorCode":     "1234",
		"companyName":  "Ayşe Yılmaz Gıda",
		"fileName":     "ayse-cv.pdf",
		"hostName":     "panel.koala",
		"contactEmail": "x",
		"user_phone":   "y",
		"ibanNumber":   "z",
		"refreshToken": "t",
		"apiKey":       "k",
		"soyadi":       "Yılmaz",
		"tcKimlikNo":   "10000000146",
	}
	got, err := audit.MaskBody(body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for k := range body {
		if !strings.Contains(s, `"`+k+`":"***"`) {
			t.Errorf("masked body %s: %s is not masked", s, k)
		}
	}
}

// The composed keys the old exception list let through are masked: the
// exception list was read as an ending, so anything that ended in one was
// exempt from the personal-part rule. The allow list is matched exactly,
// so a composition is not the key it ends in. The two listed keys that do
// read like personal ones are kept, because they are on the list by name.
func TestMaskBodyMasksComposedKeysThatEndInAllowedOnes(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"passwordFieldName":  "newPassword",
		"emailHostname":      "example.com",
		"secretTokenVersion": 3,
		"memberTokenVersion": 7,
		"targetRoleName":     "OWNER",
		"mustChangePassword": true,
		"tokenVersion":       7,
		"tableName":          "staff_members",
		"statusName":         "ACTIVE",
	}
	got, err := audit.MaskBody(body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, k := range []string{"passwordFieldName", "emailHostname", "secretTokenVersion", "memberTokenVersion", "targetRoleName"} {
		if !strings.Contains(s, `"`+k+`":"***"`) {
			t.Errorf("masked body %s: %s is not masked", s, k)
		}
	}
	for _, kept := range []string{
		`"mustChangePassword":true`, `"tokenVersion":7`,
		`"tableName":"staff_members"`, `"statusName":"ACTIVE"`,
	} {
		if !strings.Contains(s, kept) {
			t.Errorf("masked body %s lost %s", s, kept)
		}
	}
}

// A class ending is kept only when the key holds no personal part, so an
// identifier of a person's identity document is masked although it ends
// in "id", and so is a coordinate that ends in "type".
func TestMaskBodyNarrowsTheKeyClasses(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"nationalId":    "10000000146",
		"emailId":       "ayse@example.com",
		"addressId":     "9a1c0f10-0003-4c00-8000-000000000001",
		"latitudeType":  "EXACT",
		"deviceId":      "d-1",
		"sessionIds":    []string{"s-1", "s-2"},
		"entryCount":    3,
		"schemaVersion": 2,
		"recordType":    "STAFF",
	}
	got, err := audit.MaskBody(body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, k := range []string{"nationalId", "emailId", "addressId", "latitudeType"} {
		if !strings.Contains(s, `"`+k+`":"***"`) {
			t.Errorf("masked body %s: %s is not masked", s, k)
		}
	}
	for _, kept := range []string{
		`"deviceId":"d-1"`, `"sessionIds":["s-1","s-2"]`, `"entryCount":3`,
		`"schemaVersion":2`, `"recordType":"STAFF"`,
	} {
		if !strings.Contains(s, kept) {
			t.Errorf("masked body %s lost %s", s, kept)
		}
	}
}

// The value layer is the last defense on what the allow list let
// through: a personal value under a listed key is masked anyway, quoted
// or not.
func TestMaskBodyMasksPersonalValuesUnderAllowedKeys(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"code":       "4111 1111 1111 1111", // a card number under an enum key
		"orderCode":  "4111111111111111",    // and under a class-matched one
		"cursor":     "ayse@example.com",
		"total":      5551112233,       // a phone number as a JSON number
		"quantity":   4111111111111111, // a card number as a JSON number
		"statusName": "ACTIVE",
	}
	got, err := audit.MaskBody(body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, k := range []string{"code", "orderCode", "cursor", "total", "quantity"} {
		if !strings.Contains(s, `"`+k+`":"***"`) {
			t.Errorf("masked body %s: %s is not masked", s, k)
		}
	}
	for _, leaked := range []string{"4111", "ayse@example.com", "5551112233"} {
		if strings.Contains(s, leaked) {
			t.Errorf("masked body %s still carries %q", s, leaked)
		}
	}
	if !strings.Contains(s, `"statusName":"ACTIVE"`) {
		t.Errorf("masked body %s lost the status name", s)
	}
}

// A card number sent as a JSON number is masked at every card length.
// Decoding the body into a float64 used to round the long ones - a 17 to
// 19 digit number came back with its last digits changed, failed Luhn
// and went into the trail all but in clear - so the decoder keeps the
// digits the writer sent. 19 digit cards are real (Visa, Discover,
// UnionPay), and the key here is an allowed counter: the value layer is
// the only defense left.
func TestMaskBodyMasksLongCardNumbersSentAsNumbers(t *testing.T) {
	t.Parallel()
	// Luhn-valid at 13, 15, 16, 17, 18 and 19 digits.
	cards := []int64{
		4111111111119, 411111111111116, 4111111111111111,
		41111111111111113, 411111111111111118, 4111111111111111110,
	}
	for _, card := range cards {
		got, err := audit.MaskBody(map[string]any{"total": card})
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != `{"total":"***"}` {
			t.Errorf("MaskBody(%d) = %s, want the masked value", card, got)
		}
	}
	// The digits survive the decoder, so what is not a card stays whole.
	got, err := audit.MaskBody(map[string]any{"total": int64(4111111111111111109)})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"total":4111111111111111109}`; string(got) != want {
		t.Errorf("MaskBody = %s, want %s", got, want)
	}
}

// The body keeps its shape: the keys, the nesting and the length of every
// array survive the mask, and each leaf is judged by the key that names
// it. A reader of the trail sees which members an operation sent even
// where the values are gone.
func TestMaskBodyKeepsTheShape(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"roles": []string{"OWNER", "FINANCE_OFFICER"},
		"address": map[string]any{
			"street": "Atatürk Cd. 5",
			"city":   "İzmir",
			"count":  1,
		},
		"phones": []string{"+905551112233", "+905551112234"},
		"items": []any{
			map[string]any{"roleCode": "OWNER", "iban": "TR330006100519786457841326"},
		},
	}
	got, err := audit.MaskBody(body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{
		`"roles":["OWNER","FINANCE_OFFICER"]`,
		`"street":"***"`, `"city":"***"`, `"count":1`,
		`"phones":["***","***"]`,
		`"roleCode":"OWNER"`, `"iban":"***"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("masked body %s lost %s", s, want)
		}
	}
}

// A key that is itself personal data, as in a map keyed by e-mail
// address, is masked as a key: masking only values would print it.
func TestMaskBodyMasksPersonalKeys(t *testing.T) {
	t.Parallel()
	got, err := audit.MaskBody(map[string]any{
		"byUser": map[string]any{"ayse@example.com": map[string]any{"count": 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "ayse@example.com") {
		t.Errorf("masked body %s still carries the address as a key", s)
	}
	if !strings.Contains(s, `"count":2`) {
		t.Errorf("masked body %s lost the counter under the masked key", s)
	}
}

// A body over 8 KB after masking is replaced by a marker with its size.
func TestMaskBodyCapsTheSize(t *testing.T) {
	t.Parallel()
	got, err := audit.MaskBody(map[string]any{"cursor": strings.Repeat("x", audit.MaxBodyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > audit.MaxBodyBytes || !strings.Contains(string(got), `"truncated":true`) {
		t.Fatalf("capped body = %.80s…, want the truncation marker", got)
	}
}
