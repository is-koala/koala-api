-- name: EmailRegistered :one
-- EmailRegistered reports whether an active user has the address.
SELECT EXISTS (SELECT 1 FROM active_users WHERE email = $1) AS registered;

-- name: CountRecentRegistrationMails :one
-- CountRecentRegistrationMails counts the pending registrations, and so
-- the link mails, written for a mailbox within the budget window and
-- within the cooldown, and those written for any address within the
-- window: the global cap (ADR-0032 Karar 6).
SELECT count(*) FILTER (WHERE mailbox_key = sqlc.arg(mailbox_key)::text
                          AND created_at > sqlc.arg(window_start)::timestamptz) AS in_window,
       count(*) FILTER (WHERE mailbox_key = sqlc.arg(mailbox_key)::text
                          AND created_at > sqlc.arg(cooldown_start)::timestamptz) AS in_cooldown,
       count(*) AS everyone
FROM pending_registrations
WHERE created_at > sqlc.arg(window_start)::timestamptz;

-- name: RevokeOpenPendingRegistrations :exec
-- RevokeOpenPendingRegistrations ends the open registration of an
-- address: a new start is the resend (USR-002).
UPDATE pending_registrations
SET revoked_at = sqlc.arg(now)::timestamptz, email = NULL, first_name = NULL, last_name = NULL
WHERE email = sqlc.arg(email)::text AND consumed_at IS NULL AND revoked_at IS NULL;

-- name: InsertPendingRegistration :exec
INSERT INTO pending_registrations (id, email, mailbox_key, first_name, last_name, employer, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: MintRegistrationLink :one
-- MintRegistrationLink stores the digest of a new link token on an open
-- registration, replacing any earlier one, and returns where the mail
-- goes. No row: the registration ended before its mail was sent. The
-- names are not returned: the address is not verified yet, so the mail
-- does not carry what the requester typed (ADR-0032 Karar 3).
UPDATE pending_registrations
SET link_token_hash = sqlc.arg(token_hash)::bytea, link_expires_at = sqlc.arg(expires_at)::timestamptz
WHERE id = sqlc.arg(id) AND consumed_at IS NULL AND revoked_at IS NULL
RETURNING email::text;

-- name: GetPendingRegistrationByLinkForUpdate :one
-- GetPendingRegistrationByLinkForUpdate finds a registration by the digest
-- of its link token and locks it.
SELECT id, employer, link_expires_at, consumed_at, revoked_at
FROM pending_registrations
WHERE link_token_hash = $1
FOR UPDATE;

-- name: RevokePendingRegistration :exec
UPDATE pending_registrations
SET revoked_at = sqlc.arg(now)::timestamptz, email = NULL, first_name = NULL, last_name = NULL
WHERE id = sqlc.arg(id) AND consumed_at IS NULL AND revoked_at IS NULL;

-- name: SetRegistrationToken :exec
-- SetRegistrationToken stores the digest of a new registration token; it
-- replaces the one an earlier verification handed out.
UPDATE pending_registrations
SET registration_token_hash = sqlc.arg(token_hash)::bytea,
    registration_expires_at = sqlc.arg(expires_at)::timestamptz
WHERE id = sqlc.arg(id);

-- name: PeekRegistrationToken :one
-- PeekRegistrationToken finds the open registration of a live
-- registration token without consuming it.
SELECT id
FROM pending_registrations
WHERE registration_token_hash = sqlc.arg(token_hash)::bytea AND consumed_at IS NULL AND revoked_at IS NULL
  AND registration_expires_at > sqlc.arg(now)::timestamptz;

-- name: ConsumePendingRegistration :one
-- ConsumePendingRegistration consumes the registration of a live
-- registration token in one conditional statement: a second use, a
-- revoked and an expired token match no row (USR-004). The address and
-- the names leave the row in the same statement (KVKK data minimization);
-- the CTE reads them first and RETURNING hands them to the caller.
WITH cur AS (
    SELECT id, email, first_name, last_name
    FROM pending_registrations
    WHERE registration_token_hash = sqlc.arg(token_hash)::bytea AND consumed_at IS NULL AND revoked_at IS NULL
      AND registration_expires_at > sqlc.arg(now)::timestamptz
    FOR UPDATE
)
UPDATE pending_registrations p
SET consumed_at = sqlc.arg(now)::timestamptz, email = NULL, first_name = NULL, last_name = NULL
FROM cur
WHERE p.id = cur.id
RETURNING p.id, cur.email::text AS email, cur.first_name::text AS first_name, cur.last_name::text AS last_name, p.employer;

-- name: ListCurrentAgreements :many
-- ListCurrentAgreements lists the current version of every agreement.
SELECT document_code, version, mandatory
FROM agreement_versions
WHERE retired_at IS NULL
ORDER BY document_code;

-- name: InsertConsentRecord :exec
INSERT INTO consent_records (id, user_id, document_code, version, granted, recorded_at, ip_address, user_agent)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: PurgePendingRegistrations :execrows
-- PurgePendingRegistrations hard-deletes registrations written before the
-- bound (M2, 30 days; USR-007).
DELETE FROM pending_registrations WHERE created_at < $1;

-- name: CountRecentPasswordResets :one
-- CountRecentPasswordResets counts the reset requests, and so the reset
-- mails, of a user within the budget window and within the cooldown.
SELECT count(*) FILTER (WHERE created_at > sqlc.arg(window_start)::timestamptz) AS in_window,
       count(*) FILTER (WHERE created_at > sqlc.arg(cooldown_start)::timestamptz) AS in_cooldown
FROM password_reset_tokens
WHERE user_id = sqlc.arg(user_id);

-- name: RevokeOpenPasswordResets :exec
-- RevokeOpenPasswordResets ends the user's open reset token: before a new
-- one is issued, and on an in-app password change (USR-017).
UPDATE password_reset_tokens
SET revoked_at = sqlc.arg(now)::timestamptz
WHERE user_id = sqlc.arg(user_id) AND consumed_at IS NULL AND revoked_at IS NULL;

-- name: InsertPasswordReset :exec
INSERT INTO password_reset_tokens (id, user_id, created_at)
VALUES ($1, $2, $3);

-- name: MintPasswordReset :one
-- MintPasswordReset stores the digest of a new reset token on an open
-- request and returns its user. No row: the request ended before its mail
-- was sent.
UPDATE password_reset_tokens
SET token_digest = sqlc.arg(token_digest)::bytea, expires_at = sqlc.arg(expires_at)::timestamptz
WHERE id = sqlc.arg(id) AND consumed_at IS NULL AND revoked_at IS NULL
RETURNING user_id;

-- name: PeekPasswordReset :one
-- PeekPasswordReset finds the user of a live reset token without
-- consuming it.
SELECT user_id
FROM password_reset_tokens
WHERE token_digest = sqlc.arg(token_digest)::bytea AND consumed_at IS NULL AND revoked_at IS NULL
  AND expires_at > sqlc.arg(now)::timestamptz;

-- name: ConsumePasswordReset :one
-- ConsumePasswordReset consumes a live reset token in one conditional
-- statement, in the transaction that writes the password (USR-015).
UPDATE password_reset_tokens
SET consumed_at = sqlc.arg(now)::timestamptz
WHERE token_digest = sqlc.arg(token_digest)::bytea AND consumed_at IS NULL AND revoked_at IS NULL
  AND expires_at > sqlc.arg(now)::timestamptz
RETURNING user_id;

-- name: PurgePasswordResets :execrows
-- PurgePasswordResets hard-deletes reset requests written before the
-- bound (M2, 30 days).
DELETE FROM password_reset_tokens WHERE created_at < $1;

-- name: GetUserMailIdentity :one
-- GetUserMailIdentity is what a mail to the user needs, and its status: a
-- reset link goes to an active account only.
SELECT email, first_name, last_name, status
FROM active_users
WHERE id = $1;

-- name: GetUserPassword :one
-- GetUserPassword is what an in-app change checks: the hash, and the
-- address whose sign-in target counts the failures (ADR-0032 Karar 7).
SELECT email, password_hash
FROM active_users
WHERE id = $1;

-- name: RevokeUserSessionsForPassword :many
-- RevokeUserSessionsForPassword ends the user's open sessions but keep_id
-- after a password reset (keep_id is the nil id: every session) or change
-- (keep_id is the caller's session), ADR-0029 Karar 4.
UPDATE user_sessions
SET revoked_at = sqlc.arg(now)::timestamptz, revoke_reason = sqlc.arg(reason)::text
WHERE user_id = sqlc.arg(user_id) AND revoked_at IS NULL AND id <> sqlc.arg(keep_id)
RETURNING id;
