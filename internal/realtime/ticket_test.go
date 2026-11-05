package realtime_test

import (
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/realtime"
)

func newTickets() (*realtime.Tickets, *fakeClock, *fakeSessions) {
	clock := newFakeClock()
	sessions := newFakeSessions()
	return realtime.NewTickets(newMemoryTicketStore(), sessions, clock, slog.New(slog.DiscardHandler)), clock, sessions
}

var alicePhone = realtime.Principal{UserID: alice, SessionID: "s1", DeviceID: "phone"}

func isTicketInvalid(err error) bool {
	var e *apperr.Error
	return errors.As(err, &e) && e.Kind == apperr.KindUnauthenticated && e.Code == realtime.CodeTicketInvalid
}

// TestTicketRedeemedOnceUnderRace: of many concurrent redemptions of one
// ticket exactly one succeeds (the store's Take is atomic).
func TestTicketRedeemedOnceUnderRace(t *testing.T) {
	t.Parallel()

	tickets, _, _ := newTickets()
	issued, err := tickets.Issue(t.Context(), alicePhone)
	if err != nil {
		t.Fatal(err)
	}

	const n = 32
	results := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, results[i] = tickets.Redeem(t.Context(), issued.Value, "phone")
		})
	}
	wg.Wait()

	ok := 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case !isTicketInvalid(err):
			t.Fatalf("unexpected error %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d redemptions succeeded, want 1", ok)
	}
}

func TestTicketRedeem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		before func(clock *fakeClock, sessions *fakeSessions)
		device string
		ok     bool
	}{
		{"fresh, device presented", func(*fakeClock, *fakeSessions) {}, "phone", true},
		{"fresh, no device (browser)", func(*fakeClock, *fakeSessions) {}, "", true},
		{"one second before expiry", func(c *fakeClock, _ *fakeSessions) { c.Advance(realtime.TicketTTL - time.Second) }, "phone", true},
		{"at expiry", func(c *fakeClock, _ *fakeSessions) { c.Advance(realtime.TicketTTL) }, "phone", false},
		{"other device", func(*fakeClock, *fakeSessions) {}, "tablet", false},
		{"session revoked", func(_ *fakeClock, s *fakeSessions) { s.Revoke("s1") }, "phone", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tickets, clock, sessions := newTickets()
			issued, err := tickets.Issue(t.Context(), alicePhone)
			if err != nil {
				t.Fatal(err)
			}
			tt.before(clock, sessions)
			p, err := tickets.Redeem(t.Context(), issued.Value, tt.device)
			switch {
			case tt.ok && (err != nil || p != alicePhone):
				t.Fatalf("Redeem() = %+v, %v; want %+v", p, err, alicePhone)
			case !tt.ok && !isTicketInvalid(err):
				t.Fatalf("Redeem() error = %v, want %s", err, realtime.CodeTicketInvalid)
			}
		})
	}
}

func TestTicketIssueNeedsDeviceSession(t *testing.T) {
	t.Parallel()

	tickets, _, _ := newTickets()
	if _, err := tickets.Issue(t.Context(), realtime.Principal{UserID: alice, SessionID: "s1"}); err == nil {
		t.Fatal("Issue without a device succeeded")
	}
}

func TestTicketValuesAreDistinctAndOpaque(t *testing.T) {
	t.Parallel()

	tickets, _, _ := newTickets()
	seen := make(map[string]bool)
	for range 100 {
		issued, err := tickets.Issue(t.Context(), alicePhone)
		if err != nil {
			t.Fatal(err)
		}
		if len(issued.Value) != 43 || seen[issued.Value] {
			t.Fatalf("ticket %q: want 43 base64url characters, unique", issued.Value)
		}
		seen[issued.Value] = true
	}
}
