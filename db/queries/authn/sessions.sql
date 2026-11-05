-- name: RevokeDeviceSessions :many
-- RevokeDeviceSessions closes the open sessions of a device: a new sign-in
-- on the device replaces them (AUTH-002).
UPDATE user_sessions
SET revoked_at = sqlc.arg(now)::timestamptz, revoke_reason = 'REPLACED'
WHERE device_id = sqlc.arg(device_id) AND revoked_at IS NULL
RETURNING id;

-- name: InsertUserSession :exec
INSERT INTO user_sessions (id, user_id, device_id, created_at, last_seen_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: InsertUserRefreshToken :exec
INSERT INTO user_refresh_tokens (id, session_id, token_hash, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5);

-- name: GetUserRefreshTokenForUpdate :one
-- GetUserRefreshTokenForUpdate finds a refresh token by hash with its
-- session, locking both: two refreshes of one token are serialized, so
-- the second sees the first's consumption (reuse). The device and the
-- user are reached by LEFT JOIN (detach edges); NULL means deleted.
SELECT rt.id, rt.session_id, rt.expires_at, rt.consumed_at, rt.successor_salt,
       s.user_id, s.expires_at AS session_expires_at, s.revoked_at,
       d.device_key, u.status AS user_status
FROM user_refresh_tokens rt
JOIN user_sessions s ON s.id = rt.session_id
LEFT JOIN active_devices d ON d.id = s.device_id
LEFT JOIN active_users u ON u.id = s.user_id
WHERE rt.token_hash = $1
FOR UPDATE OF rt, s;

-- name: ConsumeUserRefreshToken :exec
UPDATE user_refresh_tokens
SET consumed_at = sqlc.arg(now)::timestamptz, successor_salt = sqlc.arg(successor_salt)::bytea
WHERE id = sqlc.arg(id);

-- name: RevokeUserSession :execrows
UPDATE user_sessions
SET revoked_at = sqlc.arg(now)::timestamptz, revoke_reason = sqlc.arg(reason)::text
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: GetUserSessionState :one
-- GetUserSessionState is the per-request session check (ADR-0015 §2): one
-- primary key lookup with the device and user rows by primary key. A
-- deleted device or user comes back NULL.
SELECT s.user_id, s.expires_at, s.revoked_at, s.last_seen_at,
       d.device_key, u.status AS user_status
FROM user_sessions s
LEFT JOIN active_devices d ON d.id = s.device_id
LEFT JOIN active_users u ON u.id = s.user_id
WHERE s.id = $1;

-- name: TouchUserSession :exec
-- TouchUserSession records activity at most once per interval: only a
-- last_seen_at older than the given bound is moved.
UPDATE user_sessions
SET last_seen_at = sqlc.arg(now)::timestamptz
WHERE id = sqlc.arg(id) AND last_seen_at < sqlc.arg(seen_before)::timestamptz;

-- name: ListOpenUserSessions :many
-- ListOpenUserSessions lists a user's sessions that are neither revoked
-- nor expired and whose device is active, newest first.
SELECT s.id, s.created_at, s.last_seen_at, s.expires_at, d.platform
FROM user_sessions s
LEFT JOIN active_devices d ON d.id = s.device_id
WHERE s.user_id = sqlc.arg(user_id) AND s.revoked_at IS NULL AND s.expires_at > sqlc.arg(now)::timestamptz AND d.id IS NOT NULL
ORDER BY s.created_at DESC, s.id DESC
LIMIT 100;

-- name: GetOpenUserSession :one
SELECT s.id, s.created_at, s.last_seen_at, s.expires_at, d.platform
FROM user_sessions s
LEFT JOIN active_devices d ON d.id = s.device_id
WHERE s.id = sqlc.arg(id) AND s.user_id = sqlc.arg(user_id) AND s.revoked_at IS NULL
  AND s.expires_at > sqlc.arg(now)::timestamptz AND d.id IS NOT NULL;

-- name: RevokeOwnUserSession :execrows
-- RevokeOwnUserSession closes one open session of the user; another
-- user's, a closed or an expired session matches no row.
UPDATE user_sessions
SET revoked_at = sqlc.arg(now)::timestamptz, revoke_reason = 'USER_REVOKED'
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND revoked_at IS NULL
  AND expires_at > sqlc.arg(now)::timestamptz;

-- name: RevokeOtherUserSessions :many
UPDATE user_sessions
SET revoked_at = sqlc.arg(now)::timestamptz, revoke_reason = 'USER_REVOKED'
WHERE user_id = sqlc.arg(user_id) AND id <> sqlc.arg(keep_id) AND revoked_at IS NULL
RETURNING id;

-- name: PurgeUserSessions :execrows
-- PurgeUserSessions hard-deletes sessions that ended before the bound,
-- with their refresh tokens and tickets (ON DELETE CASCADE).
DELETE FROM user_sessions
WHERE expires_at < sqlc.arg(before)::timestamptz OR revoked_at < sqlc.arg(before)::timestamptz;

-- name: CountRecentDeviceSessions :one
-- CountRecentDeviceSessions counts the user's sessions on a device seen
-- since a bound: the device is a known-good source (ADR-0025 §5). Sessions
-- ended for a stolen token or another device do not vouch for it.
SELECT count(*)
FROM user_sessions s
LEFT JOIN active_devices d ON d.id = s.device_id
WHERE s.user_id = sqlc.arg(user_id) AND d.device_key = sqlc.arg(device_key)::text
  AND s.last_seen_at >= sqlc.arg(since)::timestamptz
  AND (s.revoke_reason IS NULL OR s.revoke_reason NOT IN ('DEVICE_MISMATCH', 'REFRESH_REUSED'));
