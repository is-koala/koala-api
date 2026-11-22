-- name: InsertKey :execrows
-- InsertKey claims a new key. A row that already holds it (committed, or
-- inserted by a concurrent transaction that then commits) makes it a
-- no-op; the claim then locks that row (LockKey).
INSERT INTO idempotency_keys (actor, operation, key, request_hash, attempt, state, locked_until, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, 'IN_PROGRESS', $6, $7, $8)
ON CONFLICT (actor, operation, key) DO NOTHING;

-- name: LockKey :one
SELECT request_hash, attempt, state, locked_until, response, expires_at
FROM idempotency_keys
WHERE actor = $1 AND operation = $2 AND key = $3
FOR UPDATE;

-- name: RestartKey :exec
-- RestartKey claims a key whose row expired before the purge removed it:
-- the key is new again.
UPDATE idempotency_keys
SET request_hash = $4, attempt = $5, state = 'IN_PROGRESS', locked_until = $6, response = NULL,
    created_at = $7, completed_at = NULL, expires_at = $8
WHERE actor = $1 AND operation = $2 AND key = $3;

-- name: TakeOverKey :exec
-- TakeOverKey claims a key whose attempt outlived its lease without an
-- outcome: the new attempt id fences the old request's transaction.
UPDATE idempotency_keys
SET attempt = $4, locked_until = $5
WHERE actor = $1 AND operation = $2 AND key = $3;

-- name: CompleteKey :execrows
-- CompleteKey stores the response of the attempt that still holds the key.
-- Zero rows: another attempt took the key over.
UPDATE idempotency_keys
SET state = 'COMPLETED', response = $5, completed_at = $6
WHERE actor = $1 AND operation = $2 AND key = $3 AND attempt = $4 AND state = 'IN_PROGRESS';

-- name: FenceKey :execrows
-- FenceKey checks, in the operation's transaction, that its attempt still
-- holds the key, and locks the row until that transaction ends, without
-- storing a response: the fence of a success too large to keep. Zero
-- rows: another attempt took the key over.
UPDATE idempotency_keys
SET locked_until = locked_until
WHERE actor = $1 AND operation = $2 AND key = $3 AND attempt = $4 AND state = 'IN_PROGRESS';

-- name: ReleaseKey :execrows
-- ReleaseKey frees the key of an attempt that has no outcome to keep (a
-- 5xx, a transient 4xx). A row its transaction completed after all stays.
DELETE FROM idempotency_keys
WHERE actor = $1 AND operation = $2 AND key = $3 AND attempt = $4 AND state = 'IN_PROGRESS';

-- name: PurgeKeys :execrows
DELETE FROM idempotency_keys WHERE expires_at <= $1;
