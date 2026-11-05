-- name: GetUserCredentials :one
-- GetUserCredentials finds an active user by login identifier for sign-in.
SELECT id, password_hash, status
FROM active_users
WHERE email = $1;

-- name: GetUser :one
SELECT id, email, status, created_at
FROM active_users
WHERE id = $1;

-- name: InsertUser :exec
-- InsertUser adds a user. Faz 2's registration calls it; Faz 1 seeds tests
-- with it.
INSERT INTO active_users (id, email, password_hash, status, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: LockUserForSignIn :one
-- LockUserForSignIn locks the active user row FOR UPDATE. It serializes
-- sign-ins of one user (device upsert, session replacement) and, being
-- stronger than FOR SHARE, gives the guarantee of
-- softdelete.LockActiveParent for the device and session rows the
-- sign-in inserts: the user cannot be soft-deleted until it commits. The
-- hash comes back so the caller can see whether it changed since the
-- password was verified outside the lock.
SELECT id, status, password_hash
FROM active_users
WHERE id = $1
FOR UPDATE;

-- name: UpdateUserPasswordHash :exec
-- UpdateUserPasswordHash stores a rehash under current parameters.
UPDATE active_users SET password_hash = $2 WHERE id = $1;

-- name: GetDeviceForUpdate :one
-- GetDeviceForUpdate locks the user's active device with this key. The
-- caller holds the user's row lock (LockUserForSignIn).
SELECT id
FROM active_devices
WHERE user_id = $1 AND device_key = $2
FOR UPDATE;

-- name: InsertDevice :exec
INSERT INTO active_devices (id, user_id, device_key, platform, created_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: TouchDevice :exec
UPDATE active_devices
SET last_seen_at = $2, platform = coalesce(sqlc.narg(platform), platform)
WHERE id = $1;

-- name: InsertLoginAttempt :exec
INSERT INTO login_attempts (id, principal_kind, target_hash, outcome, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: CountLoginAttempts :one
-- CountLoginAttempts counts the attempts on one target by outcome.
SELECT count(*) FILTER (WHERE outcome = 'SUCCEEDED') AS succeeded,
       count(*) FILTER (WHERE outcome = 'FAILED') AS failed,
       count(*) FILTER (WHERE outcome = 'LOCKED') AS locked
FROM login_attempts
WHERE principal_kind = $1 AND target_hash = $2;

-- name: CountFailuresSinceSuccess :one
-- CountFailuresSinceSuccess is the persistent failure counter of a target:
-- its FAILED attempts after its last SUCCEEDED one (ADR-0025 §5).
SELECT count(*)
FROM login_attempts f
WHERE f.principal_kind = sqlc.arg(principal_kind) AND f.target_hash = sqlc.arg(target_hash)
  AND f.outcome = 'FAILED'
  AND f.created_at > coalesce((
      SELECT max(s.created_at) FROM login_attempts s
      WHERE s.principal_kind = sqlc.arg(principal_kind) AND s.target_hash = sqlc.arg(target_hash)
        AND s.outcome = 'SUCCEEDED'), '-infinity'::timestamptz);

-- name: NthFailureSinceSuccess :one
-- NthFailureSinceSuccess is the time of the target's failure number
-- position+1 after its last success: the start of a lock.
SELECT f.created_at
FROM login_attempts f
WHERE f.principal_kind = sqlc.arg(principal_kind) AND f.target_hash = sqlc.arg(target_hash)
  AND f.outcome = 'FAILED'
  AND f.created_at > coalesce((
      SELECT max(s.created_at) FROM login_attempts s
      WHERE s.principal_kind = sqlc.arg(principal_kind) AND s.target_hash = sqlc.arg(target_hash)
        AND s.outcome = 'SUCCEEDED'), '-infinity'::timestamptz)
ORDER BY f.created_at, f.id
OFFSET sqlc.arg(position)::bigint
LIMIT 1;

-- name: PurgeLoginAttempts :execrows
DELETE FROM login_attempts WHERE created_at < $1;
