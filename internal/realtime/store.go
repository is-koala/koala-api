package realtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/realtime/realtimedb"
)

// PostgresTickets is the TicketStore on Postgres (ADR-0018 §2): Put is an
// INSERT, Take a DELETE … RETURNING, so a ticket is redeemed at most once
// across every instance. Only the ticket's SHA-256 is stored.
type PostgresTickets struct {
	q *realtimedb.Queries
}

// NewPostgresTickets returns the store on conn, the application pool.
func NewPostgresTickets(conn realtimedb.DBTX) *PostgresTickets {
	return &PostgresTickets{q: realtimedb.New(conn)}
}

var _ TicketStore = (*PostgresTickets)(nil)

// Put stores a ticket under its hash.
func (s *PostgresTickets) Put(ctx context.Context, hash TicketHash, t Ticket) error {
	userID, err := uuid.Parse(string(t.Principal.UserID))
	if err != nil {
		return fmt.Errorf("realtime: ticket user id: %w", err)
	}
	sessionID, err := uuid.Parse(t.Principal.SessionID)
	if err != nil {
		return fmt.Errorf("realtime: ticket session id: %w", err)
	}
	err = s.q.InsertTicket(ctx, realtimedb.InsertTicketParams{
		TokenHash: hash[:], SessionID: sessionID, UserID: userID, DeviceKey: t.Principal.DeviceID,
		IssuedAt: t.IssuedAt, ExpiresAt: t.ExpiresAt,
	})
	if err != nil {
		return fmt.Errorf("realtime: insert ticket: %w", err)
	}
	return nil
}

// Take deletes and returns the ticket with hash; found is false when there
// is none, also when another Take got it first.
func (s *PostgresTickets) Take(ctx context.Context, hash TicketHash) (Ticket, bool, error) {
	row, err := s.q.TakeTicket(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, false, nil
	}
	if err != nil {
		return Ticket{}, false, fmt.Errorf("realtime: take ticket: %w", err)
	}
	return Ticket{
		Principal: Principal{UserID: UserID(row.UserID.String()), SessionID: row.SessionID.String(), DeviceID: row.DeviceKey},
		IssuedAt:  row.IssuedAt,
		ExpiresAt: row.ExpiresAt,
	}, true, nil
}

// Purge deletes tickets that expired before before. A periodic job calls
// it; an expired ticket is useless anyway (Redeem checks the time).
func (s *PostgresTickets) Purge(ctx context.Context, before time.Time) (int64, error) {
	n, err := s.q.PurgeTickets(ctx, before)
	if err != nil {
		return 0, fmt.Errorf("realtime: purge tickets: %w", err)
	}
	return n, nil
}
