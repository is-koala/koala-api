package jsontime_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/jsontime"
)

// An instant set in any zone is written in UTC with Z (K11).
func TestMarshalWritesUTC(t *testing.T) {
	t.Parallel()

	istanbul := time.FixedZone("TRT", 3*60*60)
	tests := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 8, 27, 9, 30, 0, 0, istanbul), `"2026-08-27T06:30:00Z"`},
		{time.Date(2026, 8, 27, 6, 30, 0, 123456000, time.UTC), `"2026-08-27T06:30:00.123456Z"`},
	}
	for _, tt := range tests {
		b, err := json.Marshal(struct {
			At jsontime.UTC `json:"at"`
		}{jsontime.From(tt.in)})
		if err != nil {
			t.Fatal(err)
		}
		if got := string(b); got != `{"at":`+tt.want+`}` {
			t.Errorf("Marshal(%v) = %s, want %s", tt.in, got, tt.want)
		}
		text, err := jsontime.From(tt.in).MarshalText()
		if err != nil || `"`+string(text)+`"` != tt.want {
			t.Errorf("MarshalText(%v) = %s, %v", tt.in, text, err)
		}
	}
}

func TestUnmarshalAcceptsOnlyZ(t *testing.T) {
	t.Parallel()

	var v jsontime.UTC
	if err := json.Unmarshal([]byte(`"2026-08-27T06:30:00Z"`), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Equal(time.Date(2026, 8, 27, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("decoded %v", v.Time)
	}
	for _, bad := range []string{`"2026-08-27T09:30:00+03:00"`, `"2026-08-27"`, `1700000000`, `"Z"`} {
		if err := json.Unmarshal([]byte(bad), &v); err == nil {
			t.Errorf("Unmarshal(%s) accepted", bad)
		}
	}
	if err := v.Bind("2026-08-27T09:30:00+03:00"); err == nil {
		t.Error("Bind accepted an offset")
	}
}
