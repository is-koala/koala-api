package address

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
)

// A proof opens for its caller before its expiry and for no one else; a
// changed byte of its claims or its signature, a proof under another key
// and a token of the wrong shape are all ADDRESS_PROOF_INVALID.
func TestProofOpensOnlyAsSigned(t *testing.T) {
	t.Parallel()
	p, err := newProver(tokenkeys.GeneratePepper())
	if err != nil {
		t.Fatal(err)
	}
	user, now := uuid.New(), time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	place, street := "ChIJ-x", "Moda Caddesi"
	r := Resolved{CityID: uuid.New(), PlaceID: &place, Thoroughfare: &street, Source: SourceAutocomplete,
		Point: &Point{Lat: 40.9876, Lng: 29.0254}}
	token, err := p.sign(claimsOf(r, user, now.Add(SessionTTL)))
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.open(token, user, now)
	if err != nil || c.PlaceID != place || c.Thoroughfare != street || c.Lat != "40.9876" || c.Source != string(SourceAutocomplete) {
		t.Fatalf("open = %+v, %v", c, err)
	}
	other, err := newProver(tokenkeys.GeneratePepper())
	if err != nil {
		t.Fatal(err)
	}
	body, sig, _ := strings.Cut(token, ".")
	flip := func(s string) string {
		b := []byte(s)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		return string(b)
	}
	for name, open := range map[string]func() error{
		"another caller": func() error { _, err := p.open(token, uuid.New(), now); return err },
		"expired":        func() error { _, err := p.open(token, user, now.Add(SessionTTL)); return err },
		"another key":    func() error { _, err := other.open(token, user, now); return err },
		"claims changed": func() error { _, err := p.open(flip(body)+"."+sig, user, now); return err },
		"sign changed":   func() error { _, err := p.open(body+"."+flip(sig), user, now); return err },
		"no signature":   func() error { _, err := p.open(body, user, now); return err },
	} {
		if err := open(); !proofRefused(err) {
			t.Errorf("%s: %v, want ADDRESS_PROOF_INVALID", name, err)
		}
	}
}

func proofRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), string(CodeProofInvalid))
}

// The backoff of a city's failed bounds lookup doubles from ten minutes
// and never passes a day.
func TestBackoffIsCappedAtADay(t *testing.T) {
	t.Parallel()
	for n, want := range map[int]time.Duration{
		1: 10 * time.Minute, 2: 20 * time.Minute, 3: 40 * time.Minute, 8: 1280 * time.Minute, 9: 24 * time.Hour, 50: 24 * time.Hour,
	} {
		if got := backoff(n); got != want {
			t.Errorf("backoff(%d) = %v, want %v", n, got, want)
		}
	}
}
