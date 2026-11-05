package staff

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/authzsql"
)

// rbac is the caller's resolved roles and permissions
// (authzsql.StaffChecker).
type rbac interface {
	Permissions(ctx context.Context, s authz.Subject) ([]authzsql.Grant, error)
	AssignedRoles(ctx context.Context, staffID uuid.UUID) ([]authzsql.Role, error)
}

// accounts is the caller's account (authn.StaffService).
type accounts interface {
	Me(ctx context.Context, p authn.StaffPrincipal) (authn.StaffMember, error)
}

// HTTP implements the staff operations of the generated strict server:
// getStaffMe, getStaffCapabilities and listStaffPermissions.
type HTTP struct {
	rbac     rbac
	accounts accounts
}

// NewHTTP returns the handlers.
func NewHTTP(r rbac, a accounts) *HTTP { return &HTTP{rbac: r, accounts: a} }

// errNoPrincipal means a staff handler ran without the authentication
// middleware's principal: a wiring bug, answered 500.
var errNoPrincipal = errors.New("staff: handler reached without an authenticated staff principal")

// GetStaffMe returns the caller's account, roles and permissions. The
// permissions are resolved from the role graph on this request (AUTH-012).
func (h *HTTP) GetStaffMe(ctx context.Context, _ apiv1.GetStaffMeRequestObject) (apiv1.GetStaffMeResponseObject, error) {
	p, ok := authn.StaffFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	m, err := h.accounts.Me(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("staff: me: %w", err)
	}
	roles, err := h.rbac.AssignedRoles(ctx, p.StaffID)
	if err != nil {
		return nil, fmt.Errorf("staff: me: %w", err)
	}
	grants, err := h.rbac.Permissions(ctx, authz.Staff(p.StaffID))
	if err != nil {
		return nil, fmt.Errorf("staff: me: %w", err)
	}
	out := apiv1.GetStaffMe200JSONResponse{
		ID: m.ID, Email: m.Email, MustChangePassword: m.MustChangePassword,
		Roles: make([]apiv1.StaffRole, len(roles)), Permissions: codes(grants),
	}
	for i, r := range roles {
		out.Roles[i] = apiv1.StaffRole{ID: r.ID, Code: r.Code, Name: r.Name}
	}
	return out, nil
}

// GetStaffCapabilities returns the caller's permissions and, for every
// known permission, whether the caller holds it. Both come from one
// resolution of the role graph (plan §5.5).
func (h *HTTP) GetStaffCapabilities(ctx context.Context, _ apiv1.GetStaffCapabilitiesRequestObject) (apiv1.GetStaffCapabilitiesResponseObject, error) {
	p, ok := authn.StaffFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	grants, err := h.rbac.Permissions(ctx, authz.Staff(p.StaffID))
	if err != nil {
		return nil, fmt.Errorf("staff: capabilities: %w", err)
	}
	can := map[string]bool{}
	for _, info := range authz.Permissions() {
		can[string(info.Code)] = false
	}
	held := codes(grants)
	for _, c := range held {
		can[c] = true
	}
	return apiv1.GetStaffCapabilities200JSONResponse{Permissions: held, Can: can}, nil
}

// ListStaffPermissions returns the permission catalog. The gate has
// already checked role.manage.
func (h *HTTP) ListStaffPermissions(context.Context, apiv1.ListStaffPermissionsRequestObject) (apiv1.ListStaffPermissionsResponseObject, error) {
	ps := authz.Permissions()
	out := apiv1.ListStaffPermissions200JSONResponse{Permissions: make([]apiv1.StaffPermission, len(ps))}
	for i, p := range ps {
		out.Permissions[i] = apiv1.StaffPermission{
			Code: string(p.Code), Mutating: p.Mutating, Sensitivity: apiv1.StaffPermissionSensitivity(p.Sensitivity),
		}
	}
	return out, nil
}

// codes lists the granted permission codes; Permissions returns them in
// catalog order, which is code order.
func codes(grants []authzsql.Grant) []string {
	out := make([]string, len(grants))
	for i, g := range grants {
		out[i] = string(g.Permission.Code)
	}
	return out
}
