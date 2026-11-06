package pii_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/pii"
)

// A Value leaves masked whatever the way out: every fmt verb, a field of
// a printed struct, slog and JSON. Its clear form leaves only through
// Reveal (ADR-0026).
func TestValueLeavesMasked(t *testing.T) {
	t.Parallel()
	const inClear = "+905551112233"
	v := pii.NewValue(pii.FieldPhone, uuid.New(), inClear)
	type holder struct {
		Phone pii.Value[string]
	}
	h := holder{Phone: v}

	var out []string
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%10.3s"} {
		out = append(out, fmt.Sprintf(format, v), fmt.Sprintf(format, h))
	}
	out = append(out, v.String(), v.GoString(), fmt.Sprint(v), fmt.Sprintln(h))

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("probe", slog.Any("phone", v), slog.Any("holder", h))
	out = append(out, logs.String())

	encoded, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, string(encoded))
	if string(encoded) != `{"Phone":"***"}` {
		t.Errorf("JSON = %s, want the masked value", encoded)
	}

	for _, o := range out {
		if strings.Contains(o, "5551112233") {
			t.Errorf("a Value leaked in clear: %q", o)
		}
	}
}
