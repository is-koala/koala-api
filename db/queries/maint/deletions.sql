-- name: LockDeletion :one
-- LockDeletion reads a deletion for softdelete.Restore and locks it, so
-- two restores of one deletion run one after the other.
SELECT root_table, root_id, restored_at
FROM deletions
WHERE id = $1
FOR UPDATE;

-- name: MarkDeletionRestored :exec
-- MarkDeletionRestored stamps a restored deletion. The row stays: it is
-- evidence (M1).
UPDATE deletions
SET restored_at = $2
WHERE id = $1;
