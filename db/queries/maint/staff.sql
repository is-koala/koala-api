-- name: InsertStaffMember :exec
-- InsertStaffMember opens a staff account (`koala staff create`). The
-- application role cannot insert staff (ADR-0025 §6); the first password
-- is a one-time password the member must change.
INSERT INTO staff_members (id, email, password_hash, status, must_change_password, created_at)
VALUES (@id, @email, @password_hash, 'ACTIVE', true, @created_at);

-- name: StaffRolesByCode :many
-- StaffRolesByCode resolves role codes to the roles that are not retired.
SELECT id, code
FROM staff_roles
WHERE code = ANY (@codes::text[]) AND retired_at IS NULL
ORDER BY code;

-- name: AssignStaffRole :exec
-- AssignStaffRole writes a role assignment; the database refuses one that
-- breaks separation of duties (staff_separation_of_duties).
INSERT INTO staff_role_assignments (staff_id, role_id, created_at)
VALUES (@staff_id, @role_id, @created_at);

-- name: LockStaffMemberByEmail :one
-- LockStaffMemberByEmail finds a staff account that is not soft-deleted
-- and locks it for `koala staff disable`.
SELECT id, status, token_version
FROM staff_members
WHERE email = @email AND deleted_at IS NULL
FOR UPDATE;

-- name: DisableStaffMember :one
-- DisableStaffMember takes an active account out of service. The trigger
-- staff_members_disable_bumps_token_version moves its token_version, so
-- every access and refresh token of the account dies (ADR-0025 §6).
UPDATE staff_members
SET status = 'DISABLED'
WHERE id = @id AND status = 'ACTIVE'
RETURNING token_version;
