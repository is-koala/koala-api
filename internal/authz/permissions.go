package authz

import (
	"slices"
	"strings"
)

// Permission is a staff permission code, "payout.release" (ADR-0026). The
// 25 codes are Java's StaffPermission. Each is one relation of the single
// platform object in model.fga, its code with "." replaced by "_"
// (platform:koala#payout_release); the spec names it in
// x-authz-permission. Permissions are defined by the code that checks
// them; roles and their grants are data (staff_role_permissions, whose
// CHECK lists the same codes).
type Permission string

// Sensitivity classifies what a permission exposes. It decides the
// retention of the audit rows written under it (a FINANCIAL one is kept
// 10 years, ADR-0026) and which reads are audited.
type Sensitivity string

// Sensitivities (Java PermissionSensitivity).
const (
	SensitivityNormal       Sensitivity = "NORMAL"
	SensitivityPersonalData Sensitivity = "PERSONAL_DATA"
	SensitivityFinancial    Sensitivity = "FINANCIAL"
)

// PlatformID is the id of the single platform object, platform:koala.
const PlatformID = "koala"

// Platform returns the platform object every staff permission is a
// relation of.
func Platform() Object { return Object{Type: TypePlatform, ID: PlatformID} }

// PermissionInfo describes one staff permission.
type PermissionInfo struct {
	Code Permission
	// Mutating permissions change state; reads under the others are
	// audited only when sensitive.
	Mutating    bool
	Sensitivity Sensitivity
}

// Relation is the permission's relation on the platform object.
func (p Permission) Relation() Action {
	return Action(strings.ReplaceAll(string(p), ".", "_"))
}

// Permissions returns the staff permission catalog, sorted by code.
// TestPermissionsMatchTheModel holds it equal to the platform relations of
// model.fga, the authorization integration test to the database's CHECK.
func Permissions() []PermissionInfo {
	return []PermissionInfo{
		{"audit.read", false, SensitivityPersonalData},
		{"badge.manage", true, SensitivityNormal},
		{"bank.account.verify", true, SensitivityFinancial},
		{"category.manage", true, SensitivityNormal},
		{"company.approve", true, SensitivityNormal},
		{"content.moderate", true, SensitivityNormal},
		{"dispute.resolve", true, SensitivityNormal},
		{"finance.read", false, SensitivityFinancial},
		{"flag.manage", true, SensitivityNormal},
		{"identity.review", true, SensitivityPersonalData},
		{"job.read", false, SensitivityPersonalData},
		{"note.read", false, SensitivityPersonalData},
		{"note.write", true, SensitivityPersonalData},
		{"payment.read", false, SensitivityFinancial},
		{"payment.refund", true, SensitivityFinancial},
		{"payout.reconcile", true, SensitivityFinancial},
		{"payout.release", true, SensitivityFinancial},
		{"role.manage", true, SensitivityNormal},
		{"settings.manage", true, SensitivityNormal},
		{"staff.manage", true, SensitivityPersonalData},
		{"staff.read", false, SensitivityPersonalData},
		{"user.deletion.manage", true, SensitivityPersonalData},
		{"user.read", false, SensitivityNormal},
		{"user.read_pii", false, SensitivityPersonalData},
		{"user.role.manage", true, SensitivityNormal},
	}
}

// LookupPermission returns the catalog entry of code.
func LookupPermission(code string) (PermissionInfo, bool) {
	ps := Permissions()
	i := slices.IndexFunc(ps, func(p PermissionInfo) bool { return string(p.Code) == code })
	if i < 0 {
		return PermissionInfo{}, false
	}
	return ps[i], true
}
