-- name: InsertDeletion :exec
-- InsertDeletion records one soft deletion. softdelete.Delete runs it as
-- koala_app in the caller's transaction; deletions is exemption class M1
-- and written directly, every soft-deleted table through its view.
INSERT INTO deletions (id, root_table, root_id, actor_kind, actor_id, reason, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);
