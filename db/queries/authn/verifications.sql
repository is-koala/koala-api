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
-- budget read it (USR-010). A verification whose code the job held back
-- unsent does not count (ADR-0033 Karar 4).
SELECT count(*) AS sends,
       COALESCE(min(created_at), sqlc.arg(since)::timestamptz)::timestamptz AS first_at,
       COALESCE(max(created_at), sqlc.arg(since)::timestamptz)::timestamptz AS last_at
FROM user_verifications
WHERE target_key = sqlc.arg(target_key)::bytea AND created_at > sqlc.arg(since)::timestamptz
  AND (code_sent_at IS NOT NULL OR held_at IS NULL);

-- name: UserSendsSince :one
-- UserSendsSince counts the verifications a user started on a channel
-- after since, with the first of them: the user's daily cap (ADR-0033).
-- A verification whose code was held back unsent does not count.
SELECT count(*) AS sends,
       COALESCE(min(created_at), sqlc.arg(since)::timestamptz)::timestamptz AS first_at
FROM user_verifications
WHERE user_id = sqlc.arg(user_id) AND channel = sqlc.arg(channel)::text
  AND created_at > sqlc.arg(since)::timestamptz
  AND (code_sent_at IS NOT NULL OR held_at IS NULL);

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

-- name: CountRecentCodes :one
-- CountRecentCodes counts the codes a channel sent after since, over every
-- user: the channel's global hourly cap (ADR-0033 Karar 4).
SELECT count(*)
FROM user_verifications
WHERE channel = sqlc.arg(channel)::text AND code_sent_at > sqlc.arg(since)::timestamptz;

-- name: HoldVerificationCode :one
-- HoldVerificationCode marks an open verification whose code the job
-- holds back unsent, and returns its end and whether this is its first
-- hold. A code minted by an earlier attempt whose send failed is dropped
-- with it: the job runs again only after a failed attempt, so that code
-- is taken as never delivered, and the verification spends no budget
-- until a code goes out (ADR-0033 Karar 4). No row: it ended.
WITH prev AS (
    SELECT o.id AS prev_id, o.held_at AS prev_held_at
    FROM user_verifications o
    WHERE o.id = sqlc.arg(id)::uuid AND o.confirmed_at IS NULL AND o.canceled_at IS NULL AND o.burned_at IS NULL
    FOR UPDATE
)
UPDATE user_verifications v
SET held_at = COALESCE(prev.prev_held_at, sqlc.arg(now)::timestamptz), code_digest = NULL, code_sent_at = NULL
FROM prev
WHERE v.id = prev.prev_id
RETURNING v.expires_at, (prev.prev_held_at IS NULL)::boolean AS first_hold;

-- name: GetVerificationTargetKey :one
-- GetVerificationTargetKey is the keyed target of a verification: the
-- code job takes the target's lock before it locks the row (ADR-0033
-- Karar 5).
SELECT target_key FROM user_verifications WHERE id = $1;

-- name: AbandonVerificationCode :exec
-- AbandonVerificationCode ends a held verification unsent: when its code
-- was finally due, the target's or the user's budget no longer allowed it
-- (ADR-0033 Karar 4). It reads as canceled and spends no budget.
UPDATE user_verifications
SET canceled_at = sqlc.arg(now)::timestamptz, held_at = COALESCE(held_at, sqlc.arg(now)::timestamptz), code_digest = NULL
WHERE id = sqlc.arg(id) AND confirmed_at IS NULL AND canceled_at IS NULL AND burned_at IS NULL;

-- name: LockVerificationForCode :one
-- LockVerificationForCode locks an open verification that has at least
-- the sending window of its life left and whose user is active, and
-- returns where its code goes, and what the resend budget of a held one
-- needs. No row: none of that holds, and no code is minted.
-- softdelete:inner-by-design users: a code goes only to a verification whose user is still active; a deleted user's verification is dead and its target must get nothing (ADR-0033)
SELECT v.user_id, v.channel, v.target, v.target_key, v.held_at, v.code_sent_at
FROM user_verifications v
LEFT JOIN active_users u ON u.id = v.user_id
WHERE v.id = sqlc.arg(id) AND v.confirmed_at IS NULL AND v.canceled_at IS NULL AND v.burned_at IS NULL
  AND v.expires_at > sqlc.arg(send_before)::timestamptz AND u.id IS NOT NULL AND u.status = 'ACTIVE'
FOR UPDATE OF v;

-- name: MintVerificationCode :exec
-- MintVerificationCode stores the digest of a new code on the
-- verification LockVerificationForCode locked, replacing any earlier one.
-- The transaction commits before the code leaves (no transaction is open
-- during the send).
UPDATE user_verifications
SET code_digest = sqlc.arg(code_digest)::bytea, code_sent_at = sqlc.arg(now)::timestamptz
WHERE id = sqlc.arg(id);

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

-- name: LockPhoneClaim :exec
-- LockPhoneClaim serializes the confirmations of one number until the
-- transaction ends, so the number's holder read under it stays the holder
-- (ADR-0033 Karar 10). The two-key form keeps it apart from the
-- single-key start lock of the same target.
SELECT pg_advisory_xact_lock(sqlc.arg(lock_class)::integer, sqlc.arg(lock_key)::integer);

-- name: GetPhoneHolder :one
-- GetPhoneHolder is the other active account that holds a number.
SELECT id
FROM active_users
WHERE phone = sqlc.arg(phone)::text AND id <> sqlc.arg(user_id);

-- name: ClearUserPhone :exec
-- ClearUserPhone takes a number away from the account that held it: the
-- number was confirmed on another account (ADR-0033 Karar 10).
UPDATE active_users SET phone = NULL, phone_verified = false WHERE id = $1;

-- name: SetUserPhone :exec
-- SetUserPhone writes a confirmed number (USR-014). A number another
-- active account holds fails users_phone_key.
UPDATE active_users SET phone = sqlc.arg(phone)::text, phone_verified = true WHERE id = sqlc.arg(id);

-- name: PurgeVerifications :execrows
-- PurgeVerifications hard-deletes verifications written before the bound
-- (M2, two days).
DELETE FROM user_verifications WHERE created_at < $1;
