-- name: InsertAuditEntry :exec
-- InsertAuditEntry appends one row of the staff audit trail. The database
-- computes retain_until (0041) and checks that a result row closes a
-- pending row of the same actor and action.
INSERT INTO admin_audit_entries (
    id, created_at, outcome, result_of, actor_kind, actor_id, action, permission, basis,
    target_type, target_id, request_id, mutating, sensitivity)
VALUES (
    @id, @created_at, @outcome, sqlc.narg(result_of), @actor_kind, sqlc.narg(actor_id), @action,
    sqlc.narg(permission), sqlc.narg(basis), sqlc.narg(target_type), sqlc.narg(target_id),
    sqlc.narg(request_id), @mutating, @sensitivity);

-- name: InsertAuditDetail :exec
-- InsertAuditDetail stores the masked body and the client address of an
-- entry (kept 6 months).
INSERT INTO admin_audit_entry_details (entry_id, created_at, body, client_key)
VALUES (@entry_id, @created_at, sqlc.narg(body), sqlc.narg(client_key));
