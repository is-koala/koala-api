package staff

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/db/maintdb"
)

// relationSink records relationship changes in the writer's transaction
// (authzsql.ChangeLog).
type relationSink interface {
	Record(ctx context.Context, q db.Querier, changes ...authz.RelationChange) error
}

// store is the koala_maint side of staff management. It is the only
// writer of the role assignment table, and every write it makes there
// emits its authz.RelationChange in the same transaction (plan §5.2-5,
// ADR-0024 §10; internal/archtest TestRelationWritesEmitChanges).
type store struct {
	sink relationSink
}

// member is a staff account as staff management sees it.
type member struct {
	ID           uuid.UUID
	Status       string
	TokenVersion int64
}

func (s store) insertMember(ctx context.Context, q db.Querier, id uuid.UUID, email, hash string, now time.Time) error {
	err := maintdb.New(q).InsertStaffMember(ctx, maintdb.InsertStaffMemberParams{ID: id, Email: email, PasswordHash: hash, CreatedAt: now})
	if err != nil {
		return fmt.Errorf("staff: insert member: %w", err)
	}
	return nil
}

// assignRoles assigns the roles named by codes to staffID. A code that is
// unknown or retired is an error; the database refuses an assignment that
// breaks separation of duties.
func (s store) assignRoles(ctx context.Context, q db.Querier, staffID uuid.UUID, codes []string, actor authz.Subject, now time.Time) error {
	m := maintdb.New(q)
	roles, err := m.StaffRolesByCode(ctx, codes)
	if err != nil {
		return fmt.Errorf("staff: load roles: %w", err)
	}
	for _, c := range codes {
		if !slices.ContainsFunc(roles, func(r maintdb.StaffRolesByCodeRow) bool { return r.Code == c }) {
			return fmt.Errorf("staff: role %q does not exist or is retired", c)
		}
	}
	changes := make([]authz.RelationChange, 0, len(roles))
	for _, r := range roles {
		if err := m.AssignStaffRole(ctx, maintdb.AssignStaffRoleParams{StaffID: staffID, RoleID: r.ID, CreatedAt: now}); err != nil {
			return fmt.Errorf("staff: assign role %s: %w", r.Code, err)
		}
		changes = append(changes, authz.RelationChange{
			Op: authz.ChangeWrite, Actor: actor,
			Tuple: authz.Tuple{
				Object:   authz.Object{Type: authz.TypeStaffRole, ID: r.ID.String()},
				Relation: string(authz.ActionStaffRoleAssignee),
				User:     authz.TupleUser{Type: authz.TypeStaff, ID: staffID.String()},
			},
		})
	}
	if err := s.sink.Record(ctx, q, changes...); err != nil {
		return fmt.Errorf("staff: record role assignments: %w", err)
	}
	return nil
}

// findMember locks the account with the login address email; false when
// there is none (or it is soft-deleted).
func (s store) findMember(ctx context.Context, q db.Querier, email string) (member, bool, error) {
	row, err := maintdb.New(q).LockStaffMemberByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return member{}, false, nil
	}
	if err != nil {
		return member{}, false, fmt.Errorf("staff: find member: %w", err)
	}
	return member{ID: row.ID, Status: row.Status, TokenVersion: row.TokenVersion}, true, nil
}

// disable takes an active account out of service and returns its new
// token version (the database moves it).
func (s store) disable(ctx context.Context, q db.Querier, id uuid.UUID) (int64, error) {
	v, err := maintdb.New(q).DisableStaffMember(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("staff: disable member: %w", err)
	}
	return v, nil
}
