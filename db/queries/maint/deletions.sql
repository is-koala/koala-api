-- name: LockDeletion :one
-- LockDeletion reads a deletion for softdelete.Restore and locks it, so
-- two restores of one deletion run one after the other.
SELECT root_table, root_id, restored_at
FROM deletions
WHERE id = $1
FOR UPDATE;

-- name: MarkDeletionRestored :execrows
-- MarkDeletionRestored stamps a deletion's restore once: an already
-- restored row is not touched (0 rows), and the casts make restored_at and
-- restored_by_kind non-null parameters. The row stays: it is evidence (M1).
UPDATE deletions
SET restored_at = @restored_at::timestamptz,
    restored_by_kind = @restored_by_kind::text,
    restored_by = sqlc.narg(restored_by)::uuid
WHERE id = @id AND restored_at IS NULL;
