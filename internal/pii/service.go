package pii

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/pii/piidb"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// checker is the staff decision the gate needs, read in the caller's
// transaction (authzsql.StaffChecker).
type checker interface {
	CheckIn(ctx context.Context, q db.Querier, s authz.Subject, a authz.Action, o authz.Object) (authz.Decision, error)
}

// Gate opens personal data to staff (plan §5.3). The database stamps the
// access row's time (0041).
type Gate struct {
	checker checker
}

// NewGate returns the gate.
func NewGate(c checker) *Gate { return &Gate{checker: c} }

// Open decides whether staff may see fields of dataSubject for purpose
// and, if so, writes the pii_access_log row in q's transaction and returns
// the grant. The decision is the user.read_pii permission; its basis goes
// into the row.
//
// A denial is (nil, nil): the answer is masked, not refused (Java
// AdminDetailManager, PI-088). When the row cannot be written the grant is
// nil and the error says why: no row, no grant (plan §5.5). The row is
// written under a savepoint, so the caller's transaction survives the
// failure and can still answer masked.
//
// The grant is to be used after q's transaction commits: the log row
// becomes durable with the commit, and a rollback takes it away. A caller
// builds the response with the grant only once InTx has returned nil, so
// no personal value leaves without its committed access row.
func (g *Gate) Open(ctx context.Context, q db.Querier, staff authz.Subject, dataSubject uuid.UUID, p Purpose, fields []Field) (*Grant, error) {
	switch {
	case staff.Kind != authz.SubjectStaff:
		return nil, errors.New("pii: only staff open personal data")
	case dataSubject == uuid.Nil:
		return nil, errors.New("pii: no data subject")
	case !p.Valid():
		return nil, fmt.Errorf("pii: purpose %q is not lower snake case", p)
	case len(fields) == 0:
		return nil, errors.New("pii: no field to open")
	}
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		if !slices.Contains(knownFields(), f) {
			return nil, fmt.Errorf("pii: unknown field %q", f)
		}
		names = append(names, string(f))
	}
	d, err := g.checker.CheckIn(ctx, q, staff, authz.ActionPlatformUserReadPii, authz.Platform())
	if err != nil {
		return nil, fmt.Errorf("pii: decide %s: %w", authz.ActionPlatformUserReadPii, err)
	}
	if !d.Allowed {
		return nil, nil
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("pii: grant id: %w", err)
	}
	var requestID, route *string
	if rid := httpx.RequestIDFrom(ctx); rid != "" {
		requestID = &rid
	}
	if rc := chi.RouteContext(ctx); rc != nil && rc.RoutePattern() != "" {
		pattern := rc.RoutePattern()
		route = &pattern
	}
	if err := g.log(ctx, q, piidb.InsertPIIAccessParams{
		ID: id, StaffID: staff.ID, Basis: d.Basis.String(),
		DataSubjectID: dataSubject, Purpose: string(p), Fields: names, Route: route, RequestID: requestID,
	}); err != nil {
		return nil, err
	}
	return &Grant{id: id, staff: staff.ID, subject: dataSubject, fields: slices.Clone(fields)}, nil
}

// log writes the access row under a savepoint of q's transaction.
func (g *Gate) log(ctx context.Context, q db.Querier, row piidb.InsertPIIAccessParams) error {
	sp, err := q.PgxTx().Begin(ctx)
	if err != nil {
		return fmt.Errorf("pii: savepoint: %w", err)
	}
	if err := piidb.New(sp).InsertPIIAccess(ctx, row); err != nil {
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return errors.Join(fmt.Errorf("pii: write the access log: %w", err), fmt.Errorf("pii: roll back the savepoint: %w", rbErr))
		}
		return fmt.Errorf("pii: write the access log: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("pii: release the savepoint: %w", err)
	}
	return nil
}
