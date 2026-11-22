-- name: LockVerificationTarget :exec
-- LockVerificationTarget serializes the starts of one target, over every
-- user, until the transaction ends: the resend budget counts the target's
-- rows and then writes one, so two concurrent starts cannot both pass it
-- (ADR-0033). The key is the first eight bytes of the target's keyed
-- hash.
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key)::bigint);

-- name: TargetSendsSince :one
-- TargetSendsSince counts the verifications started for a keyed target,
-- by any user, after since, with the first and the last of them (since
-- itself when there is none). The resend cooldown and the target's hourly
-- budget read it (USR-010).
SELECT count(*) AS sends,
       COALESCE(min(created_at), sqlc.arg(since)::timestamptz)::timestamptz AS first_at,
       COALESCE(max(created_at), sqlc.arg(since)::timestamptz)::timestamptz AS last_at
FROM user_verifications
WHERE target_key = sqlc.arg(target_key)::bytea AND created_at > sqlc.arg(since)::timestamptz;

-- name: UserSendsSince :one
-- UserSendsSince counts the verifications a user started on a channel
-- after since, with the first of them: the user's daily cap (ADR-0033).
SELECT count(*) AS sends,
       COALESCE(min(created_at), sqlc.arg(since)::timestamptz)::timestamptz AS first_at
FROM user_verifications
WHERE user_id = sqlc.arg(user_id) AND channel = sqlc.arg(channel)::text
  AND created_at > sqlc.arg(since)::timestamptz;

-- name: CancelOpenVerifications :exec
-- CancelOpenVerifications ends the user's open verification of a channel
-- and target: a new start replaces it, and its code stops working
-- (USR-010).
UPDATE user_verifications
SET canceled_at = sqlc.arg(now)::timestamptz, code_digest = NULL
WHERE user_id = sqlc.arg(user_id) AND channel = sqlc.arg(channel)::text AND target = sqlc.arg(target)::text
  AND confirmed_at IS NULL AND canceled_at IS NULL AND burned_at IS NULL;

-- name: InsertVerification :exec
INSERT INTO user_verifications (id, user_id, channel, target, target_key, created_at, expires_at, failed_attempts)
VALUES ($1, $2, $3, $4, $5, $6, $7, 0);

-- name: CountRecentTextMessages :one
-- CountRecentTextMessages counts the codes sent by text message after
-- since, over every user: the global hourly cap (ADR-0033).
SELECT count(*)
FROM user_verifications
WHERE channel = 'PHONE' AND code_sent_at > sqlc.arg(since)::timestamptz;

-- name: MintVerificationCode :one
-- MintVerificationCode stores the digest of a new code on an open,
-- unexpired verification, replacing any earlier one, and returns where
-- the code goes. No row: the verification ended or expired before its
-- code was sent. The statement commits on its own, before the code
-- leaves (no transaction is open during the send).
UPDATE user_verifications
SET code_digest = sqlc.arg(code_digest)::bytea, code_sent_at = sqlc.arg(now)::timestamptz
WHERE id = sqlc.arg(id) AND confirmed_at IS NULL AND canceled_at IS NULL AND burned_at IS NULL
  AND expires_at > sqlc.arg(now)::timestamptz
RETURNING user_id, channel, target;

-- name: LockVerification :one
-- LockVerification locks one of the user's verifications. Another user's
-- verification matches no row, so it is neither seen nor touched
-- (USR-011); the lock serializes the confirm attempts of one
-- verification, so the attempt limit holds under concurrency (USR-009).
SELECT id, channel, target, created_at, expires_at, code_digest, failed_attempts,
       confirmed_at, canceled_at, burned_at
FROM user_verifications
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
FOR UPDATE;

-- name: RecordFailedVerification :one
-- RecordFailedVerification counts a wrong code. The attempt that reaches
-- the limit burns the verification and clears its digest in the same
-- statement (USR-009).
UPDATE user_verifications
SET failed_attempts = failed_attempts + 1,
    burned_at = CASE WHEN failed_attempts + 1 >= sqlc.arg(max_attempts)::integer THEN sqlc.arg(now)::timestamptz END,
    code_digest = CASE WHEN failed_attempts + 1 >= sqlc.arg(max_attempts)::integer THEN NULL ELSE code_digest END
WHERE id = sqlc.arg(id)
RETURNING failed_attempts;

-- name: ConfirmVerification :exec
-- ConfirmVerification consumes a verification: it is confirmed and its
-- digest leaves the row (USR-013).
UPDATE user_verifications
SET confirmed_at = sqlc.arg(now)::timestamptz, code_digest = NULL
WHERE id = sqlc.arg(id);

-- name: CancelVerification :execrows
-- CancelVerification ends one of the user's pending verifications;
-- an ended one, or another user's, matches no row.
UPDATE user_verifications
SET canceled_at = sqlc.arg(now)::timestamptz, code_digest = NULL
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
  AND confirmed_at IS NULL AND canceled_at IS NULL AND burned_at IS NULL;

-- name: GetVerification :one
-- GetVerification reads one of the user's verifications that the user
-- can see: a burned and a canceled one match no row, like another
-- user's (USR-011).
SELECT id, channel, target, created_at, expires_at, failed_attempts, confirmed_at
FROM user_verifications
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND canceled_at IS NULL AND burned_at IS NULL;

-- name: ListOpenVerifications :many
-- ListOpenVerifications lists the user's open verifications, newest
-- first, after the keyset position (created_at, id): neither confirmed,
-- canceled nor burned, and not expired.
SELECT id, channel, target, created_at, expires_at, failed_attempts, confirmed_at
FROM user_verifications
WHERE user_id = sqlc.arg(user_id)
  AND confirmed_at IS NULL AND canceled_at IS NULL AND burned_at IS NULL
  AND expires_at > sqlc.arg(now)::timestamptz
  AND (created_at, id) < (sqlc.arg(before_at)::timestamptz, sqlc.arg(before_id)::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(max_rows)::integer;

-- name: ListOpenVerificationsNewer :many
-- ListOpenVerificationsNewer is ListOpenVerifications backwards: the open
-- verifications newer than the keyset position, oldest first, for the
-- page before it.
SELECT id, channel, target, created_at, expires_at, failed_attempts, confirmed_at
FROM user_verifications
WHERE user_id = sqlc.arg(user_id)
  AND confirmed_at IS NULL AND canceled_at IS NULL AND burned_at IS NULL
  AND expires_at > sqlc.arg(now)::timestamptz
  AND (created_at, id) > (sqlc.arg(after_at)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(max_rows)::integer;

-- name: SetUserEmail :exec
-- SetUserEmail writes a confirmed address (USR-014). An address another
-- active account holds fails users_email_key.
UPDATE active_users SET email = sqlc.arg(email)::text, email_verified = true WHERE id = sqlc.arg(id);

-- name: SetUserPhone :exec
-- SetUserPhone writes a confirmed number (USR-014). A number another
-- active account holds fails users_phone_key.
UPDATE active_users SET phone = sqlc.arg(phone)::text, phone_verified = true WHERE id = sqlc.arg(id);

-- name: PurgeVerifications :execrows
-- PurgeVerifications hard-deletes verifications written before the bound
-- (M2, two days).
DELETE FROM user_verifications WHERE created_at < $1;
