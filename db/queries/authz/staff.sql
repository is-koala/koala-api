-- name: StaffRoleGraph :many
-- StaffRoleGraph is the one recursive query a staff decision reads (plan
-- §5.5, ADR-0026): every role the member reaches, from a direct
-- assignment (kind assignee) or through the inclusion of a reached parent
-- (kind includer, value the parent's id), and the permissions of every
-- reached role (kind permission, value the code).
-- Retired roles neither grant nor pass anything on, and a soft-deleted or
-- DISABLED member reaches nothing. internal/authz/authzsql
-- turns the rows into the member's relationship tuples; the evaluator
-- decides on them, so a revoked assignment counts on the next request.
WITH RECURSIVE reached (role_id, parent_role_id) AS (
    SELECT a.role_id, NULL::uuid
    FROM staff_role_assignments a
    JOIN staff_roles r ON r.id = a.role_id
    LEFT JOIN active_staff_members m ON m.id = a.staff_id
    WHERE a.staff_id = @staff_id::uuid AND r.retired_at IS NULL AND m.status = 'ACTIVE'
  UNION
    SELECT i.child_role_id, i.parent_role_id
    FROM reached
    JOIN staff_role_includes i ON i.parent_role_id = reached.role_id
    JOIN staff_roles r ON r.id = i.child_role_id
    WHERE r.retired_at IS NULL
)
SELECT 'assignee'::text AS kind, reached.role_id::uuid AS role_id, ''::text AS value
FROM reached
WHERE reached.parent_role_id IS NULL
UNION ALL
SELECT 'includer', reached.role_id, reached.parent_role_id::text
FROM reached
WHERE reached.parent_role_id IS NOT NULL
UNION ALL
SELECT 'permission', p.role_id, p.permission
FROM staff_role_permissions p
WHERE p.role_id IN (SELECT reached.role_id FROM reached);

-- name: AssignedStaffRoles :many
-- AssignedStaffRoles lists the roles assigned to a staff member directly,
-- for /v1/admin/me; inherited permissions come from StaffRoleGraph.
SELECT r.id, r.code, r.name
FROM staff_role_assignments a
JOIN staff_roles r ON r.id = a.role_id
WHERE a.staff_id = @staff_id::uuid AND r.retired_at IS NULL
ORDER BY r.code;

-- name: InsertAuthzChange :exec
-- InsertAuthzChange records one relationship change (authz.RelationSink).
INSERT INTO authz_change_log (id, created_at, op, tuple, actor_kind, actor_id, request_id)
VALUES (@id, @created_at, @op, @tuple, @actor_kind, sqlc.narg(actor_id), sqlc.narg(request_id));
