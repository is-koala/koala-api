// Package staff is the staff side of the admin surface (ADR-0026): the
// x-authz-permission gate of /v1/admin, the caller's own identity and
// capabilities, the permission catalog, and staff management through the
// `koala staff` command (koala_maint). Staff management over the API
// lands in Faz 8.
package staff

import (
	"regexp"
	"slices"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// Error codes of the staff surface (api/components/problem.yaml).
const (
	// CodePermissionDenied: the caller lacks the operation's
	// x-authz-permission (403, AUTH-012).
	CodePermissionDenied apperr.Code = "STAFF_PERMISSION_DENIED"
)

// Codes lists the error codes this package returns, for the contract test.
func Codes() []apperr.Code { return []apperr.Code{CodePermissionDenied} }

// ExtPermission and ExtExempt are the authorization extensions of a staff
// operation in the contract (style guide §11).
const (
	ExtPermission = "x-authz-permission"
	ExtExempt     = "x-authz-exempt"
)

// Actions of the `koala staff` command in the audit trail.
const (
	ActionCreate      = "staff.member.create"
	ActionDisable     = "staff.member.disable"
	ActionRoleGrant   = "staff.member.role.grant"
	ownerRoleCode     = "OWNER"
	maxRolesPerCreate = 10
)

// OwnerRole is the role of the first super admin (`koala staff create
// --role OWNER`).
const OwnerRole = ownerRoleCode

var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// validEmail reports whether email is a plausible login address; the
// database checks the normalized form again.
func validEmail(email string) bool {
	return len(email) >= 3 && len(email) <= 254 && emailPattern.MatchString(email)
}

// distinctRoles reports whether codes is a non-empty list of distinct
// role codes of reasonable length.
func distinctRoles(codes []string) bool {
	if len(codes) == 0 || len(codes) > maxRolesPerCreate {
		return false
	}
	sorted := slices.Clone(codes)
	slices.Sort(sorted)
	return len(slices.Compact(sorted)) == len(codes)
}
