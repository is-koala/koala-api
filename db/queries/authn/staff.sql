-- name: GetStaffCredentials :one
-- GetStaffCredentials finds an active (not soft-deleted) staff member by
-- login identifier. A DISABLED account is returned: its sign-in fails
-- exactly like a wrong password (AUTH-010).
SELECT id, password_hash
FROM active_staff_members
WHERE email = $1;

-- name: GetStaffPasswordHash :one
SELECT password_hash
FROM active_staff_members
WHERE id = $1;

-- name: LockStaffForSignIn :one
-- LockStaffForSignIn locks the staff row: the failure counter and the lock
-- are read and written under it, and it gives the guarantee of
-- softdelete.LockActiveParent for the session the sign-in inserts.
SELECT id, password_hash, status, must_change_password, failed_login_attempts,
       backoff_until, token_version
FROM active_staff_members
WHERE id = $1
FOR UPDATE;

-- name: SetStaffFailures :exec
-- SetStaffFailures stores the account-wide failure counter and backoff.
UPDATE active_staff_members
SET failed_login_attempts = sqlc.arg(failed_login_attempts), backoff_until = sqlc.narg(backoff_until)
WHERE id = sqlc.arg(id);

-- name: GetStaffLoginLock :one
-- GetStaffLoginLock reads and locks the (account, address) pair's counter.
SELECT failures, locked_until
FROM staff_login_locks
WHERE staff_id = $1 AND client_key = $2
FOR UPDATE;

-- name: SetStaffLoginLock :exec
INSERT INTO staff_login_locks (staff_id, client_key, failures, locked_until, updated_at)
VALUES (sqlc.arg(staff_id), sqlc.arg(client_key), sqlc.arg(failures), sqlc.narg(locked_until), sqlc.arg(now)::timestamptz)
ON CONFLICT (staff_id, client_key) DO UPDATE
SET failures = EXCLUDED.failures, locked_until = EXCLUDED.locked_until, updated_at = EXCLUDED.updated_at;

-- name: ClearStaffLoginLock :exec
DELETE FROM staff_login_locks WHERE staff_id = $1 AND client_key = $2;

-- name: PurgeStaffLoginLocks :execrows
-- PurgeStaffLoginLocks deletes pairs untouched since the bound whose lock,
-- if any, has ended.
DELETE FROM staff_login_locks
WHERE updated_at < sqlc.arg(before)::timestamptz
  AND (locked_until IS NULL OR locked_until < sqlc.arg(before)::timestamptz);

-- name: UpdateStaffPasswordHash :exec
-- UpdateStaffPasswordHash stores a rehash under current parameters; it is
-- not a password change.
UPDATE active_staff_members SET password_hash = $2 WHERE id = $1;

-- name: ChangeStaffPassword :exec
-- ChangeStaffPassword sets a new password: the must-change flag drops and
-- the token version moves, which ends every token issued before.
UPDATE active_staff_members
SET password_hash = sqlc.arg(password_hash), must_change_password = false,
    password_changed_at = sqlc.arg(now)::timestamptz,
    token_version = token_version + 1
WHERE id = sqlc.arg(id);

-- name: BumpStaffTokenVersion :exec
UPDATE active_staff_members SET token_version = token_version + 1 WHERE id = $1;

-- name: GetStaffMember :one
SELECT id, email, status, must_change_password, created_at
FROM active_staff_members
WHERE id = $1;

-- name: InsertStaffSession :exec
INSERT INTO staff_sessions (id, staff_id, token_version, created_at, last_seen_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: InsertStaffRefreshToken :exec
INSERT INTO staff_refresh_tokens (id, session_id, token_hash, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5);

-- name: GetStaffRefreshTokenForUpdate :one
-- GetStaffRefreshTokenForUpdate finds a staff refresh token by hash with
-- its session, both locked. The member is reached by LEFT JOIN (detach
-- edge); NULL means deleted.
SELECT rt.id, rt.session_id, rt.expires_at, rt.consumed_at, rt.successor_salt,
       s.staff_id, s.token_version, s.expires_at AS session_expires_at, s.revoked_at,
       m.token_version AS member_token_version, m.status AS member_status
FROM staff_refresh_tokens rt
JOIN staff_sessions s ON s.id = rt.session_id
LEFT JOIN active_staff_members m ON m.id = s.staff_id
WHERE rt.token_hash = $1
FOR UPDATE OF rt, s;

-- name: ConsumeStaffRefreshToken :exec
UPDATE staff_refresh_tokens
SET consumed_at = sqlc.arg(now)::timestamptz, successor_salt = sqlc.arg(successor_salt)::bytea
WHERE id = sqlc.arg(id);

-- name: RevokeStaffSession :exec
UPDATE staff_sessions
SET revoked_at = sqlc.arg(now)::timestamptz, revoke_reason = sqlc.arg(reason)::text
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: GetStaffSessionState :one
-- GetStaffSessionState is the per-request staff check: the session and the
-- member's current token version, status and must-change flag.
SELECT s.staff_id, s.token_version, s.expires_at, s.revoked_at, s.last_seen_at,
       m.token_version AS member_token_version, m.status AS member_status,
       m.must_change_password
FROM staff_sessions s
LEFT JOIN active_staff_members m ON m.id = s.staff_id
WHERE s.id = $1;

-- name: TouchStaffSession :exec
UPDATE staff_sessions
SET last_seen_at = sqlc.arg(now)::timestamptz
WHERE id = sqlc.arg(id) AND last_seen_at < sqlc.arg(seen_before)::timestamptz;

-- name: PurgeStaffSessions :execrows
DELETE FROM staff_sessions
WHERE expires_at < sqlc.arg(before)::timestamptz OR revoked_at < sqlc.arg(before)::timestamptz;
