package authzsql

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/authzdb"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// ChangeLog is the RelationSink of today (plan §5.2-5, ADR-0024 §10): it
// appends every relationship change to authz_change_log in the writer's
// transaction. On migration day an outbox sink takes its place.
type ChangeLog struct {
	model *authz.Model
	clock clock.Clock
}

var _ authz.RelationSink = (*ChangeLog)(nil)

// NewChangeLog returns the sink; changes are validated against model.
func NewChangeLog(model *authz.Model, clk clock.Clock) *ChangeLog {
	return &ChangeLog{model: model, clock: clk}
}

// Record implements authz.RelationSink.
func (l *ChangeLog) Record(ctx context.Context, q db.Querier, changes ...authz.RelationChange) error {
	now := l.clock.Now()
	var requestID *string
	if id := httpx.RequestIDFrom(ctx); id != "" {
		requestID = &id
	}
	for i := range changes {
		ch := &changes[i]
		var op string
		switch ch.Op {
		case authz.ChangeWrite:
			op = "WRITE"
		case authz.ChangeDelete:
			op = "DELETE"
		case authz.ChangeInvalid:
			return errors.New("authz: relation change without an operation")
		default:
			return fmt.Errorf("authz: relation change of unknown operation %d", ch.Op)
		}
		if err := l.model.ValidateTuple(ch.Tuple); err != nil {
			return fmt.Errorf("authz: relation change: %w", err)
		}
		kind, actor, err := actorOf(ch.Actor)
		if err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("authz: relation change id: %w", err)
		}
		err = authzdb.New(q).InsertAuthzChange(ctx, authzdb.InsertAuthzChangeParams{
			ID: id, CreatedAt: now, Op: op, Tuple: ch.Tuple.String(),
			ActorKind: kind, ActorID: actor, RequestID: requestID,
		})
		if err != nil {
			return fmt.Errorf("authz: record relation change %s: %w", ch.Tuple, err)
		}
	}
	return nil
}

// actorOf maps the actor of a change to its kind and id: a staff member,
// or the system (the `koala staff` command, jobs) without an id.
func actorOf(s authz.Subject) (string, uuid.NullUUID, error) {
	switch s.Kind {
	case authz.SubjectStaff:
		return "staff", uuid.NullUUID{UUID: s.ID, Valid: true}, nil
	case authz.SubjectSystem:
		return "system", uuid.NullUUID{}, nil
	case authz.SubjectInvalid, authz.SubjectUser:
	}
	return "", uuid.NullUUID{}, fmt.Errorf("authz: relationship changes are made by staff or the system, not by %d", s.Kind)
}
