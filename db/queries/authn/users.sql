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
-- sign-in inserts: the user cannot be soft-deleted until it commits.
SELECT id, status
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
INSERT INTO login_attempts (id, principal_kind, target_hash, succeeded, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: CountLoginAttempts :one
-- CountLoginAttempts counts the attempts on one target by outcome.
SELECT count(*) FILTER (WHERE succeeded) AS succeeded, count(*) FILTER (WHERE NOT succeeded) AS failed
FROM login_attempts
WHERE principal_kind = $1 AND target_hash = $2;

-- name: PurgeLoginAttempts :execrows
DELETE FROM login_attempts WHERE created_at < $1;
