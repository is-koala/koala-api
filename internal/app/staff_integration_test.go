//go:build integration

package app

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/staff"
)

// staffID returns the id of the test staff account.
func (a *authApp) staffID(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := a.maint.Pool().QueryRow(t.Context(), "SELECT id FROM staff_members WHERE email = $1", staffEmail).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// grantRole assigns role to the test staff account as koala_maint.
func (a *authApp) grantRole(t *testing.T, role string) {
	t.Helper()
	_, err := a.maint.Pool().Exec(t.Context(), `
		INSERT INTO staff_role_assignments (staff_id, role_id, created_at)
		SELECT $1, id, now() FROM staff_roles WHERE code = $2`, a.staffID(t), role)
	if err != nil {
		t.Fatal(err)
	}
}

func (a *authApp) revokeRoles(t *testing.T) {
	t.Helper()
	if _, err := a.maint.Pool().Exec(t.Context(), "DELETE FROM staff_role_assignments WHERE staff_id = $1", a.staffID(t)); err != nil {
		t.Fatal(err)
	}
}

type auditRow struct {
	Outcome, Action string
	Permission      *string
	ActorID         *uuid.UUID
	Basis           *string
	ResultOf        *uuid.UUID
}

func (a *authApp) auditRows(t *testing.T, action string) []auditRow {
	t.Helper()
	rows, err := a.maint.Pool().Query(t.Context(), `
		SELECT outcome, action, permission, actor_id, basis, result_of
		FROM admin_audit_entries WHERE action = $1 ORDER BY id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Outcome, &r.Action, &r.Permission, &r.ActorID, &r.Basis, &r.ResultOf); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

type staffMe struct {
	Roles []struct {
		ID   uuid.UUID `json:"id"`
		Code string    `json:"code"`
	} `json:"roles"`
	Permissions []string `json:"permissions"`
}

type capabilities struct {
	Permissions []string        `json:"permissions"`
	Can         map[string]bool `json:"can"`
}

// AUTH-012 (ADR-0026): the staff token carries no permission. The
// x-authz-permission gate resolves the caller's permissions from the role
// graph on every request: a role granted after sign-in opens the
// operation on the same token, and a revoked one closes it on the next
// request. A refusal is 403 STAFF_PERMISSION_DENIED and a DENIED audit
// row; /v1/admin/me and /v1/admin/me/capabilities show the same
// resolution.
func TestStaffPermissionGateOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, rateLimitGroups())
	a.addStaff(t, false)
	tok := a.loginStaff(t, staffPw).AccessToken

	{ // AUTH-012: refused without the permission.
		wantStatus(t, a.call(t, http.MethodGet, "/v1/admin/permissions", tok, ""), http.StatusForbidden, string(staff.CodePermissionDenied))
		rows := a.auditRows(t, "listStaffPermissions")
		if len(rows) != 1 || rows[0].Outcome != "DENIED" || rows[0].Permission == nil || *rows[0].Permission != "role.manage" ||
			rows[0].ActorID == nil || *rows[0].ActorID != a.staffID(t) || rows[0].Basis != nil {
			t.Fatalf("AUTH-012: audit rows = %+v, want one DENIED row of the caller for role.manage without basis", rows)
		}
		caps := decodeBody[capabilities](t, a.call(t, http.MethodGet, "/v1/admin/me/capabilities", tok, ""))
		if len(caps.Permissions) != 0 || len(caps.Can) != 25 || caps.Can["role.manage"] {
			t.Fatalf("AUTH-012: capabilities = %+v, want no permission and all 25 codes false", caps)
		}
	}

	a.grantRole(t, "OWNER")
	{ // AUTH-012: a role granted after sign-in counts on the same token.
		rec := a.call(t, http.MethodGet, "/v1/admin/permissions", tok, "")
		wantStatus(t, rec, http.StatusOK, "")
		if n := strings.Count(rec.Body.String(), `"code":`); n != 25 {
			t.Fatalf("catalog has %d permissions, want 25: %s", n, rec.Body)
		}
		me := decodeBody[staffMe](t, a.call(t, http.MethodGet, "/v1/admin/me", tok, ""))
		if len(me.Roles) != 1 || me.Roles[0].Code != "OWNER" || len(me.Permissions) != 25 || !slices.IsSorted(me.Permissions) {
			t.Fatalf("me = %+v, want OWNER and all 25 permissions sorted", me)
		}
		caps := decodeBody[capabilities](t, a.call(t, http.MethodGet, "/v1/admin/me/capabilities", tok, ""))
		if !caps.Can["payout.release"] || !caps.Can["user.read_pii"] || len(caps.Permissions) != 25 {
			t.Fatalf("AUTH-012: capabilities = %+v, want every permission through OWNER's inclusions", caps)
		}
	}

	a.revokeRoles(t)
	{ // AUTH-012: a revoked role counts on the next request.
		wantStatus(t, a.call(t, http.MethodGet, "/v1/admin/permissions", tok, ""), http.StatusForbidden, string(staff.CodePermissionDenied))
		me := decodeBody[staffMe](t, a.call(t, http.MethodGet, "/v1/admin/me", tok, ""))
		if len(me.Roles) != 0 || len(me.Permissions) != 0 {
			t.Fatalf("AUTH-012: me = %+v after the revocation, want no role and no permission", me)
		}
	}
}

var oneTimePassword = regexp.MustCompile(`one-time password: (\S+)`)

// `koala staff create` opens the first super admin as koala_maint with a
// one-time password that must be changed; `koala staff disable` moves the
// token version, so the member's tokens die (issue #7, ADR-0025 §6). Both
// are audited (pending, then result) and the role assignment is in the
// relation change log.
func TestStaffCommand(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, rateLimitGroups())
	env := []string{"KOALA_ENV=test", "KOALA_MAINT_DATABASE_URL=" + a.dbt.URL(db.RoleMaint)}

	code, stdout, stderr := run([]string{"staff", "create", "--email", " Ops@IsKoala.com ", "--role", "owner"}, env...)
	if code != ExitOK {
		t.Fatalf("create: exit %d: %s", code, stderr)
	}
	m := oneTimePassword.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("create printed no one-time password: %q", stdout)
	}
	if strings.Contains(stderr, m[1]) {
		t.Fatal("the one-time password reached the log")
	}
	login := a.call(t, http.MethodPost, "/v1/admin/auth/login", "", `{"email":"`+staffEmail+`","password":"`+m[1]+`"}`)
	wantStatus(t, login, http.StatusOK, "")
	pair := decodeBody[tokens](t, login)
	if !pair.MustChangePassword {
		t.Error("a new staff account must change its one-time password (AUTH-014)")
	}

	rows := a.auditRows(t, staff.ActionCreate)
	if len(rows) != 2 || rows[0].Outcome != "PENDING" || rows[1].Outcome != "SUCCEEDED" || rows[1].ResultOf == nil || rows[0].ActorID != nil {
		t.Fatalf("create audit rows = %+v, want a system PENDING row and its SUCCEEDED result", rows)
	}
	var changes int
	err := a.maint.Pool().QueryRow(t.Context(), "SELECT count(*) FROM admin_audit_entry_details WHERE body ->> 'email' = '***'").Scan(&changes)
	if err != nil || changes != 1 {
		t.Fatalf("masked detail rows = %d, %v; want the create body with its address masked", changes, err)
	}
	var tuple string
	err = a.dbt.Open(t, db.RoleOwner).Pool().QueryRow(t.Context(),
		"SELECT tuple FROM authz_change_log WHERE op = 'WRITE' AND actor_kind = 'system'").Scan(&tuple)
	if err != nil || !strings.HasSuffix(tuple, "#assignee@staff:"+a.staffID(t).String()) {
		t.Fatalf("relation change = %q, %v; want the OWNER assignment", tuple, err)
	}

	if code, _, stderr := run([]string{"staff", "create", "--email", "second@iskoala.com", "--role", "MODERATOR", "--role", "FINANCE_ANALYST"}, env...); code != ExitError {
		t.Fatalf("separation of duties: exit %d, want %d: %s", code, ExitError, stderr)
	}

	code, stdout, stderr = run([]string{"staff", "disable", "--email", staffEmail}, env...)
	if code != ExitOK || !strings.Contains(stdout, "disabled") {
		t.Fatalf("disable: exit %d: %s %s", code, stdout, stderr)
	}
	wantStatus(t, a.call(t, http.MethodGet, "/v1/admin/me", pair.AccessToken, ""), http.StatusUnauthorized, string(authn.CodeStaffSessionEnded))
	refresh := a.call(t, http.MethodPost, "/v1/admin/auth/refresh", "", `{"refreshToken":"`+pair.RefreshToken+`"}`)
	if refresh.Code != http.StatusUnauthorized {
		t.Fatalf("refresh after disable: %d %s", refresh.Code, refresh.Body)
	}
	if code, _, _ := run([]string{"staff", "disable", "--email", staffEmail}, env...); code != ExitError {
		t.Fatal("disabling a disabled account succeeded")
	}
}
