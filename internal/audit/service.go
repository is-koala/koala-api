package audit

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/audit/auditdb"
	"github.com/is-koala/koala-api/internal/authz"
)

// Recorder writes audit rows. It writes on the DBTX it is given: the
// caller decides the transaction (a pending row commits on its own, before
// the mutation; a result row may share the mutation's transaction). The
// database stamps every row with the statement's time and derives its
// retention from it; no client clock moves a retention (0041).
type Recorder struct{}

// NewRecorder returns a recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// Pending writes the PENDING row of a mutation and returns its id. The
// mutation runs only when this succeeded.
func (r *Recorder) Pending(ctx context.Context, q auditdb.DBTX, e Entry) (uuid.UUID, error) {
	if !e.Mutating {
		return uuid.Nil, errors.New("audit: a pending row is written for a mutation only")
	}
	return r.insert(ctx, q, e, OutcomePending, uuid.Nil)
}

// Result closes the pending row with the mutation's outcome. e is the
// entry the pending row was written with.
func (r *Recorder) Result(ctx context.Context, q auditdb.DBTX, pending uuid.UUID, e Entry, succeeded bool) error {
	if !e.Mutating || pending == uuid.Nil {
		return errors.New("audit: a result closes the pending row of a mutation")
	}
	o := OutcomeFailed
	if succeeded {
		o = OutcomeSucceeded
	}
	_, err := r.insert(ctx, q, e, o, pending)
	return err
}

// Record writes a single row: a refusal (DENIED, without basis) or the
// outcome of a read. A mutation's outcome goes through Pending and Result.
func (r *Recorder) Record(ctx context.Context, q auditdb.DBTX, e Entry, o Outcome) (uuid.UUID, error) {
	switch o {
	case OutcomeDenied:
		e.Basis = nil
	case OutcomeSucceeded, OutcomeFailed:
		if e.Mutating {
			return uuid.Nil, errors.New("audit: the outcome of a mutation closes its pending row (Result)")
		}
	case OutcomePending:
		return uuid.Nil, errors.New("audit: a pending row is written with Pending")
	default:
		return uuid.Nil, fmt.Errorf("audit: unknown outcome %q", o)
	}
	return r.insert(ctx, q, e, o, uuid.Nil)
}

// Detail stores the personal part of an entry: body with its personal data
// masked and capped (MaskBody), and the client address key. Either may be
// empty.
func (r *Recorder) Detail(ctx context.Context, q auditdb.DBTX, entry uuid.UUID, body any, clientKey string) error {
	var masked []byte
	if body != nil {
		var err error
		if masked, err = MaskBody(body); err != nil {
			return err
		}
	}
	err := auditdb.New(q).InsertAuditDetail(ctx, auditdb.InsertAuditDetailParams{
		EntryID: entry, Body: masked, ClientKey: optional(clientKey),
	})
	if err != nil {
		return fmt.Errorf("audit: write detail of %s: %w", entry, err)
	}
	return nil
}

func (r *Recorder) insert(ctx context.Context, q auditdb.DBTX, e Entry, o Outcome, resultOf uuid.UUID) (uuid.UUID, error) {
	kind, actor, err := actorOf(e.Actor)
	if err != nil {
		return uuid.Nil, err
	}
	if e.Sensitivity == "" {
		e.Sensitivity = authz.SensitivityNormal
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("audit: entry id: %w", err)
	}
	p := auditdb.InsertAuditEntryParams{
		ID: id, Outcome: string(o),
		ActorKind: kind, ActorID: actor, Action: e.Action,
		Permission: optional(string(e.Permission)), RequestID: optional(e.RequestID),
		Mutating: e.Mutating, Sensitivity: string(e.Sensitivity),
	}
	if resultOf != uuid.Nil {
		p.ResultOf = uuid.NullUUID{UUID: resultOf, Valid: true}
	}
	if len(e.Basis) > 0 {
		p.Basis = optional(e.Basis.String())
	}
	if e.Target != (authz.Object{}) {
		p.TargetType, p.TargetID = optional(e.Target.Type), optional(e.Target.ID)
	}
	if err := auditdb.New(q).InsertAuditEntry(ctx, p); err != nil {
		return uuid.Nil, fmt.Errorf("audit: write %s entry of %s: %w", o, e.Action, err)
	}
	return id, nil
}

// actorOf maps the actor to the actor columns: a staff member, or the
// system without an id.
func actorOf(s authz.Subject) (string, uuid.NullUUID, error) {
	switch s.Kind {
	case authz.SubjectStaff:
		return "staff", uuid.NullUUID{UUID: s.ID, Valid: true}, nil
	case authz.SubjectSystem:
		return "system", uuid.NullUUID{}, nil
	case authz.SubjectInvalid, authz.SubjectUser:
	}
	return "", uuid.NullUUID{}, fmt.Errorf("audit: the actor of a staff audit row is staff or the system, not %d", s.Kind)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
