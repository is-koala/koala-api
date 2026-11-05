package httpx_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/httpx"
)

func TestIdempotencyKey(t *testing.T) {
	t.Parallel()

	const key = "0192f3c4-7b1a-7cc0-9a51-3f2b8e4d6a10"
	tests := []struct {
		name    string
		header  string
		wantKey string
		wantOK  bool
	}{
		{"bare value", key, key, true},
		{"structured-field string", `"` + key + `"`, key, true},
		{"surrounding spaces", "  " + key + " ", key, true},
		{"absent", "", "", false},
		{"too short", "abc", "", false},
		{"too long", strings.Repeat("a", 256), "", false},
		{"one quote", `"` + key, "", false},
		{"inner quote", `"ab"cdefgh"`, "", false},
		{"space inside", "abcd efgh", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := http.Header{}
			if tt.header != "" {
				h.Set(httpx.HeaderIdempotencyKey, tt.header)
			}
			got, ok := httpx.IdempotencyKey(h)
			if got != tt.wantKey || ok != tt.wantOK {
				t.Fatalf("IdempotencyKey(%q) = %q, %v; want %q, %v", tt.header, got, ok, tt.wantKey, tt.wantOK)
			}
		})
	}
}
