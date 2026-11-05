package realtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// TicketTTL is how long an issued ticket can be redeemed (ADR-0011).
const TicketTTL = 60 * time.Second

// ticketBytes is the entropy of a ticket; its value is the unpadded
// base64url encoding.
const (
	ticketBytes = 32
	ticketLen   = 43
)

// Clock tells the time. Faz 1's clock.Clock satisfies it.
type Clock interface {
	Now() time.Time
}

// Principal is the authenticated device session a ticket is bound to and a
// connection acts as.
type Principal struct {
	UserID    UserID
	SessionID string
	DeviceID  string
}

// Ticket is what the store keeps for an issued ticket. The store never
// sees the ticket value, only its hash.
type Ticket struct {
	Principal Principal
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// TicketHash is the SHA-256 of a ticket value. A ticket has 256 bits of
// entropy, so a fast hash is enough (same reasoning as refresh tokens,
// plan §3.9); a leaked store yields no usable ticket.
type TicketHash [sha256.Size]byte

// TicketStore keeps tickets between issue and redemption. Take must be
// atomic: of two concurrent Takes of one hash, at most one finds it. That
// is the single-use guarantee.
//
// Faz 4 implements it on Postgres: Put is an INSERT, Take is
// DELETE FROM realtime_tickets WHERE hash = $1 RETURNING ..., which keeps
// redemption single-use across instances; a River periodic job deletes
// expired rows.
type TicketStore interface {
	Put(ctx context.Context, hash TicketHash, t Ticket) error
	// Take removes and returns the ticket. found is false when there is
	// none, also when it was already taken.
	Take(ctx context.Context, hash TicketHash) (t Ticket, found bool, err error)
}

// Sessions reports whether a session is still active on its device. A
// ticket outlives nothing: revocation takes effect at redemption too
// (ADR-0015 §2). Faz 1's session store implements it.
type Sessions interface {
	Active(ctx context.Context, p Principal) (bool, error)
}

// IssuedTicket is a ticket value handed to the client once.
type IssuedTicket struct {
	Value     string
	ExpiresAt time.Time
}

// Tickets issues and redeems WebSocket tickets.
type Tickets struct {
	store    TicketStore
	sessions Sessions
	clock    Clock
	logger   *slog.Logger
}

// NewTickets returns the ticket service.
func NewTickets(store TicketStore, sessions Sessions, clock Clock, logger *slog.Logger) *Tickets {
	return &Tickets{store: store, sessions: sessions, clock: clock, logger: logger}
}

// Issue returns a new ticket bound to p's user, session and device.
func (t *Tickets) Issue(ctx context.Context, p Principal) (IssuedTicket, error) {
	if p.UserID == "" || p.SessionID == "" || p.DeviceID == "" {
		return IssuedTicket{}, errors.New("realtime: issue ticket: principal lacks user, session or device")
	}
	var b [ticketBytes]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+)
	value := base64.RawURLEncoding.EncodeToString(b[:])

	now := t.clock.Now()
	tk := Ticket{Principal: p, IssuedAt: now, ExpiresAt: now.Add(TicketTTL)}
	if err := t.store.Put(ctx, hashTicket(value), tk); err != nil {
		return IssuedTicket{}, fmt.Errorf("realtime: store ticket: %w", err)
	}
	return IssuedTicket{Value: value, ExpiresAt: tk.ExpiresAt}, nil
}

// Redeem consumes a ticket and returns the principal it was issued to.
// presentedDevice is the X-Device-Id of the handshake, or "" when there is
// none: browsers cannot set headers on a WebSocket, so the binding is
// enforced at issue time and by the session check here, and a native
// client that presents a device must present the ticket's.
//
// Every rejection is the same 401 REALTIME_TICKET_INVALID, so a client
// cannot tell an unknown ticket from a used or expired one. The ticket is
// consumed even when the rejection comes after the lookup.
func (t *Tickets) Redeem(ctx context.Context, value, presentedDevice string) (Principal, error) {
	invalid := apperr.New(apperr.KindUnauthenticated, CodeTicketInvalid)
	if !validTicketValue(value) {
		t.reject(ctx, "malformed")
		return Principal{}, invalid
	}
	tk, found, err := t.store.Take(ctx, hashTicket(value))
	if err != nil {
		return Principal{}, fmt.Errorf("realtime: take ticket: %w", err)
	}
	switch {
	case !found:
		t.reject(ctx, "unknown_or_used")
		return Principal{}, invalid
	case !t.clock.Now().Before(tk.ExpiresAt):
		t.reject(ctx, "expired")
		return Principal{}, invalid
	case presentedDevice != "" && presentedDevice != tk.Principal.DeviceID:
		t.reject(ctx, "device_mismatch")
		return Principal{}, invalid
	}
	active, err := t.sessions.Active(ctx, tk.Principal)
	if err != nil {
		return Principal{}, fmt.Errorf("realtime: check session: %w", err)
	}
	if !active {
		t.reject(ctx, "session_inactive")
		return Principal{}, invalid
	}
	return tk.Principal, nil
}

func (t *Tickets) reject(ctx context.Context, reason string) {
	t.logger.InfoContext(ctx, "realtime ticket rejected", slog.String("reason", reason))
}

func hashTicket(value string) TicketHash {
	return sha256.Sum256([]byte(value))
}

func validTicketValue(v string) bool {
	if len(v) != ticketLen {
		return false
	}
	for _, c := range []byte(v) {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// MemoryTicketStore is a TicketStore for one process. It is the spike's
// store (ADR-0011) and is replaced by the Postgres store in Faz 4; with
// more than one instance a ticket issued on one could not be redeemed on
// another. Expired tickets are dropped on Put.
type MemoryTicketStore struct {
	mu      sync.Mutex
	tickets map[TicketHash]Ticket
	// order is the insertion order. With one TTL for every ticket it is
	// also the expiry order, so pruning stops at the first live ticket.
	order []TicketHash
}

// NewMemoryTicketStore returns an empty store.
func NewMemoryTicketStore() *MemoryTicketStore {
	return &MemoryTicketStore{tickets: make(map[TicketHash]Ticket)}
}

// Put stores t under hash.
func (s *MemoryTicketStore) Put(_ context.Context, hash TicketHash, t Ticket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(t.IssuedAt)
	if _, dup := s.tickets[hash]; dup {
		return errors.New("realtime: duplicate ticket hash")
	}
	s.tickets[hash] = t
	s.order = append(s.order, hash)
	return nil
}

// Take removes and returns the ticket stored under hash.
func (s *MemoryTicketStore) Take(_ context.Context, hash TicketHash) (Ticket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, found := s.tickets[hash]
	delete(s.tickets, hash)
	return t, found, nil
}

// Len returns the number of stored tickets, expired ones included until
// the next Put.
func (s *MemoryTicketStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tickets)
}

func (s *MemoryTicketStore) prune(now time.Time) {
	i := 0
	for ; i < len(s.order); i++ {
		t, found := s.tickets[s.order[i]]
		if found && now.Before(t.ExpiresAt) {
			break
		}
		delete(s.tickets, s.order[i])
	}
	s.order = s.order[i:]
}
