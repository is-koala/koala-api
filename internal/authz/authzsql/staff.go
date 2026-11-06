// Package authzsql holds the typed SQL sources of authz (plan §5.2-1,
// §5.6-5; ADR-0024 §6, ADR-0026): tuple sources that read relationship
// tables in the caller's transaction, and the relation change log. It is
// the only reader of the relationship tables for authorization; depguard
// keeps their generated queries (authzdb) inside internal/authz.
//
// The first source is staff RBAC. One recursive query loads the staff
// member's part of the role graph (StaffRoleGraph); the generic evaluator
// decides on it. That keeps a single decision point and gives the Basis
// with the roles a decision went through, while the database is read once
// per request, as plan §5.5 asks. The subgraph is complete for its
// subject: a role the member does not reach cannot grant the member
// anything (Zanzibar's Leopard index flattens nested groups the same
// way), so decisions and bases equal those over every tuple.
package authzsql

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/authzdb"
	"github.com/is-koala/koala-api/internal/platform/db"
)

// Kinds of StaffRoleGraph rows.
const (
	rowAssignee   = "assignee"
	rowIncluder   = "includer"
	rowPermission = "permission"
)

// StaffTuples is the tuple source of one staff member: the tuples of the
// roles the member reaches. It answers only for that member; asked about
// another staff member it fails rather than deny silently.
type StaffTuples struct {
	staff authz.TupleUser
	users map[string][]authz.TupleUser
	all   []authz.Tuple
}

var _ authz.TupleReader = (*StaffTuples)(nil)

// LoadStaffTuples reads staffID's role subgraph with one query on q.
func LoadStaffTuples(ctx context.Context, q authzdb.DBTX, staffID uuid.UUID) (*StaffTuples, error) {
	rows, err := authzdb.New(q).StaffRoleGraph(ctx, staffID)
	if err != nil {
		return nil, fmt.Errorf("authz: load the staff role graph: %w", err)
	}
	t := &StaffTuples{
		staff: authz.TupleUser{Type: authz.TypeStaff, ID: staffID.String()},
		users: map[string][]authz.TupleUser{},
	}
	for _, r := range rows {
		role := authz.Object{Type: authz.TypeStaffRole, ID: r.RoleID.String()}
		var tp authz.Tuple
		switch r.Kind {
		case rowAssignee:
			tp = authz.Tuple{Object: role, Relation: string(authz.ActionStaffRoleAssignee), User: t.staff}
		case rowIncluder:
			tp = authz.Tuple{Object: role, Relation: string(authz.ActionStaffRoleIncluder),
				User: authz.TupleUser{Type: authz.TypeStaffRole, ID: r.Value}}
		case rowPermission:
			info, ok := authz.LookupPermission(r.Value)
			if !ok {
				return nil, fmt.Errorf("authz: role %s grants unknown permission %q", role.ID, r.Value)
			}
			tp = authz.Tuple{Object: authz.Platform(), Relation: string(info.Code.Relation()),
				User: authz.TupleUser{Type: authz.TypeStaffRole, ID: role.ID, Relation: string(authz.ActionStaffRoleAssignee)}}
		default:
			return nil, fmt.Errorf("authz: staff role graph row of kind %q", r.Kind)
		}
		t.add(tp)
	}
	return t, nil
}

func (t *StaffTuples) add(tp authz.Tuple) {
	k := key(tp.Object, tp.Relation)
	if slices.Contains(t.users[k], tp.User) {
		return
	}
	t.users[k] = append(t.users[k], tp.User)
	t.all = append(t.all, tp)
}

// Tuples returns the loaded tuples sorted by their OpenFGA form.
func (t *StaffTuples) Tuples() []authz.Tuple {
	out := slices.Clone(t.all)
	slices.SortFunc(out, func(a, b authz.Tuple) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// Contains implements authz.TupleReader.
func (t *StaffTuples) Contains(_ context.Context, o authz.Object, relation string, user authz.TupleUser) (bool, error) {
	if user.Type == authz.TypeStaff && user != t.staff {
		return false, fmt.Errorf("authz: staff tuples of %s asked about %s", t.staff, user)
	}
	return slices.Contains(t.users[key(o, relation)], user), nil
}

// Read implements authz.TupleReader.
func (t *StaffTuples) Read(_ context.Context, o authz.Object, relation string) ([]authz.TupleUser, error) {
	return slices.Clone(t.users[key(o, relation)]), nil
}

func key(o authz.Object, relation string) string { return o.String() + "#" + relation }

// StaffChecker decides staff permissions (platform#<permission>) and role
// membership (staff_role#assignee) from the database: one StaffRoleGraph
// query per check, then the evaluator. It answers for staff subjects;
// a marketplace user holds no staff permission (D5) and is denied without
// a query. Other object types are refused: staff RBAC knows nothing about
// marketplace objects.
type StaffChecker struct {
	model *authz.Model
	db    authzdb.DBTX
}

var _ authz.Checker = (*StaffChecker)(nil)

// NewStaffChecker returns the staff checker reading on database (a pool,
// or a transaction with In).
func NewStaffChecker(model *authz.Model, database authzdb.DBTX) *StaffChecker {
	return &StaffChecker{model: model, db: database}
}

// In returns the checker reading in q, the caller's transaction.
func (c *StaffChecker) In(q authzdb.DBTX) *StaffChecker {
	return &StaffChecker{model: c.model, db: q}
}

// CheckIn is Check reading in q, the caller's transaction: a decision
// that sees the transaction's own writes (pii.Gate).
func (c *StaffChecker) CheckIn(ctx context.Context, q db.Querier, s authz.Subject, a authz.Action, o authz.Object) (authz.Decision, error) {
	return c.In(q).Check(ctx, s, a, o)
}

// evaluator loads s's tuples and returns the evaluator over them.
func (c *StaffChecker) evaluator(ctx context.Context, s authz.Subject) (*authz.Evaluator, error) {
	var reader authz.TupleReader
	if s.Kind == authz.SubjectStaff {
		tuples, err := LoadStaffTuples(ctx, c.db, s.ID)
		if err != nil {
			return nil, err
		}
		reader = tuples
	} else {
		reader = noTuples{}
	}
	return authz.NewEvaluator(c.model, reader), nil
}

func staffObject(o authz.Object) bool {
	return o.Type == authz.TypePlatform || o.Type == authz.TypeStaffRole
}

// Check implements authz.Checker.
func (c *StaffChecker) Check(ctx context.Context, s authz.Subject, a authz.Action, o authz.Object, opts ...authz.CheckOption) (authz.Decision, error) {
	if !staffObject(o) {
		return authz.Decision{Conceal: true}, fmt.Errorf("authz: staff checker asked about %s", o)
	}
	ev, err := c.evaluator(ctx, s)
	if err != nil {
		return authz.Decision{}, err
	}
	d, err := ev.Check(ctx, s, a, o, opts...)
	if err != nil {
		return d, fmt.Errorf("authz: staff check: %w", err)
	}
	return d, nil
}

// CheckBatch implements authz.Checker with one query for the batch.
func (c *StaffChecker) CheckBatch(ctx context.Context, s authz.Subject, a authz.Action, objs []authz.Object, opts ...authz.CheckOption) (map[authz.Object]authz.Decision, error) {
	for _, o := range objs {
		if !staffObject(o) {
			return nil, fmt.Errorf("authz: staff checker asked about %s", o)
		}
	}
	ev, err := c.evaluator(ctx, s)
	if err != nil {
		return nil, err
	}
	out, err := ev.CheckBatch(ctx, s, a, objs, opts...)
	if err != nil {
		return nil, fmt.Errorf("authz: staff check: %w", err)
	}
	return out, nil
}

// Grant is one staff permission a subject holds, with the basis.
type Grant struct {
	Permission authz.PermissionInfo
	Basis      authz.Basis
}

// Permissions decides every staff permission for s with one query: the
// held ones, in catalog order, each with its basis. The capabilities
// endpoint is built from it, so every value comes from Check (plan §5.5).
func (c *StaffChecker) Permissions(ctx context.Context, s authz.Subject) ([]Grant, error) {
	ev, err := c.evaluator(ctx, s)
	if err != nil {
		return nil, err
	}
	var out []Grant
	for _, p := range authz.Permissions() {
		d, err := ev.Check(ctx, s, p.Code.Relation(), authz.Platform())
		if err != nil {
			return nil, fmt.Errorf("authz: staff permission %s: %w", p.Code, err)
		}
		if d.Allowed {
			out = append(out, Grant{Permission: p, Basis: d.Basis})
		}
	}
	return out, nil
}

// Role is a role assigned to a staff member directly.
type Role struct {
	ID   uuid.UUID
	Code string
	Name string
}

// AssignedRoles lists the roles assigned to staffID directly, by code.
func (c *StaffChecker) AssignedRoles(ctx context.Context, staffID uuid.UUID) ([]Role, error) {
	rows, err := authzdb.New(c.db).AssignedStaffRoles(ctx, staffID)
	if err != nil {
		return nil, fmt.Errorf("authz: load assigned staff roles: %w", err)
	}
	out := make([]Role, len(rows))
	for i, r := range rows {
		out[i] = Role{ID: r.ID, Code: r.Code, Name: r.Name}
	}
	return out, nil
}

// noTuples is the empty source: a subject that is not staff reaches no
// role.
type noTuples struct{}

func (noTuples) Contains(context.Context, authz.Object, string, authz.TupleUser) (bool, error) {
	return false, nil
}

func (noTuples) Read(context.Context, authz.Object, string) ([]authz.TupleUser, error) {
	return nil, nil
}
