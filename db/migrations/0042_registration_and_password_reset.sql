-- Three-step registration, password reset and the in-app password change
-- (ADR-0029 Karar 2 and 4, ADR-0032).
--
-- users gains the registration fields. phone_verified and
-- identity_verified keep their default: a new account is never verified
-- by phone or identity document (USR-006); the other columns lose theirs,
-- so every insert states them.
--
-- pending_registrations, password_reset_tokens: short-lived (M2). Both
-- tokens of a pending registration and the reset token are opaque, stored
-- as SHA-256 and minted when their mail is sent, so no plaintext token is
-- ever written to the database, the job queue included (ADR-0032).
--
-- agreement_versions (M4) is the catalog of agreements a registration
-- consents to; consent_records (M1) is the evidence of each decision and
-- is never updated or deleted.

-- +goose Up
DROP VIEW active_users;
ALTER TABLE users
    ADD COLUMN first_name text NOT NULL DEFAULT '',
    ADD COLUMN last_name text NOT NULL DEFAULT '',
    ADD COLUMN employer boolean NOT NULL DEFAULT false,
    ADD COLUMN email_verified boolean NOT NULL DEFAULT false,
    ADD COLUMN phone_verified boolean NOT NULL DEFAULT false,
    ADD COLUMN identity_verified boolean NOT NULL DEFAULT false;
ALTER TABLE users
    ALTER COLUMN first_name DROP DEFAULT,
    ALTER COLUMN last_name DROP DEFAULT,
    ALTER COLUMN employer DROP DEFAULT,
    ALTER COLUMN email_verified DROP DEFAULT;
-- +koala soft-delete-view v1 users security_barrier
-- +koala begin generated (make generate); do not edit
CREATE VIEW active_users WITH (security_barrier) AS
    SELECT * FROM users WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_users TO koala_app;
-- +koala end generated

-- A password reset ends every session, a password change every other one
-- (ADR-0029 Karar 4). Neither kind of ended session vouches for its device
-- as a known-good source afterwards (db/queries/authn/sessions.sql).
ALTER TABLE user_sessions DROP CONSTRAINT user_sessions_revoke_reason_check;
ALTER TABLE user_sessions ADD CONSTRAINT user_sessions_revoke_reason_check CHECK (revoke_reason IN (
    'LOGOUT', 'REPLACED', 'USER_REVOKED', 'DEVICE_MISMATCH', 'REFRESH_REUSED', 'EXPIRED', 'INACTIVE',
    'PASSWORD_RESET', 'PASSWORD_CHANGED'));

-- One registration between its first and its third step. The link token
-- (step 1, one hour) and the registration token (step 2, fifteen minutes)
-- are SHA-256 digests; a NULL link means its mail has not been sent yet.
-- At most one open row per address; a new start revokes the open one.
-- Rows are hard-deleted 30 days after they were written (USR-007).
CREATE TABLE pending_registrations (
    id uuid PRIMARY KEY,
    -- Trimmed and lower-cased by the application, like users.email.
    email text NOT NULL,
    first_name text NOT NULL,
    last_name text NOT NULL,
    employer boolean NOT NULL,
    created_at timestamptz NOT NULL,
    link_token_hash bytea,
    link_expires_at timestamptz,
    registration_token_hash bytea,
    registration_expires_at timestamptz,
    consumed_at timestamptz,
    revoked_at timestamptz,
    CONSTRAINT pending_registrations_email_check CHECK (email = lower(btrim(email)) AND length(email) BETWEEN 3 AND 254),
    CONSTRAINT pending_registrations_names_check CHECK (
        length(first_name) BETWEEN 1 AND 100 AND length(last_name) BETWEEN 1 AND 100),
    CONSTRAINT pending_registrations_link_check CHECK (
        (link_token_hash IS NULL) = (link_expires_at IS NULL)
        AND (link_token_hash IS NULL OR octet_length(link_token_hash) = 32)),
    CONSTRAINT pending_registrations_registration_check CHECK (
        (registration_token_hash IS NULL) = (registration_expires_at IS NULL)
        AND (registration_token_hash IS NULL OR octet_length(registration_token_hash) = 32)),
    CONSTRAINT pending_registrations_ended_check CHECK (consumed_at IS NULL OR revoked_at IS NULL),
    CONSTRAINT pending_registrations_link_key UNIQUE (link_token_hash),
    CONSTRAINT pending_registrations_registration_key UNIQUE (registration_token_hash)
);

CREATE UNIQUE INDEX pending_registrations_open_email_key ON pending_registrations (email)
    WHERE consumed_at IS NULL AND revoked_at IS NULL;
-- The per-address mail budget counts an address's recent rows.
CREATE INDEX pending_registrations_email_created_idx ON pending_registrations (email, created_at);
CREATE INDEX pending_registrations_created_at_idx ON pending_registrations (created_at);

-- One password reset request of a user. The token is a SHA-256 digest of
-- 32 random bytes, minted when the mail is sent (NULL until then), and
-- lives fifteen minutes. At most one open token per user; consuming it is
-- one conditional UPDATE in the transaction that writes the password
-- (USR-015). Rows are hard-deleted 30 days after they were written.
CREATE TABLE password_reset_tokens (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    token_digest bytea,
    expires_at timestamptz,
    consumed_at timestamptz,
    revoked_at timestamptz,
    CONSTRAINT password_reset_tokens_digest_check CHECK (
        (token_digest IS NULL) = (expires_at IS NULL)
        AND (token_digest IS NULL OR octet_length(token_digest) = 32)),
    CONSTRAINT password_reset_tokens_ended_check CHECK (consumed_at IS NULL OR revoked_at IS NULL),
    CONSTRAINT password_reset_tokens_consumed_check CHECK (consumed_at IS NULL OR token_digest IS NOT NULL),
    CONSTRAINT password_reset_tokens_digest_key UNIQUE (token_digest)
);

CREATE UNIQUE INDEX password_reset_tokens_open_user_key ON password_reset_tokens (user_id)
    WHERE consumed_at IS NULL AND revoked_at IS NULL;
CREATE INDEX password_reset_tokens_user_created_idx ON password_reset_tokens (user_id, created_at);
CREATE INDEX password_reset_tokens_created_at_idx ON password_reset_tokens (created_at);

-- The agreements a registration consents to. A document has one current
-- version; a new version retires the old one, it is never deleted, so the
-- consents given to it keep their reference. Seeded with Java's versions
-- (koala-backend 2026-08-27-13-seed-agreement-versions).
CREATE TABLE agreement_versions (
    document_code text NOT NULL,
    version text NOT NULL,
    mandatory boolean NOT NULL,
    effective_from timestamptz NOT NULL,
    retired_at timestamptz,
    PRIMARY KEY (document_code, version),
    CONSTRAINT agreement_versions_document_code_check CHECK (document_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    CONSTRAINT agreement_versions_version_check CHECK (length(version) BETWEEN 1 AND 32)
);

CREATE UNIQUE INDEX agreement_versions_current_key ON agreement_versions (document_code)
    WHERE retired_at IS NULL;

INSERT INTO agreement_versions (document_code, version, mandatory, effective_from) VALUES
    ('TERMS_OF_SERVICE', '1.0', true, '2026-08-27T00:00:00Z'),
    ('PRIVACY_POLICY', '1.0', true, '2026-08-27T00:00:00Z'),
    ('EXPLICIT_CONSENT', '1.0', true, '2026-08-27T00:00:00Z'),
    ('COMMERCIAL_MESSAGE', '1.0', false, '2026-08-27T00:00:00Z');

-- One consent decision of a user, given at registration: evidence, never
-- updated or deleted. The account's anonymization keeps it (koala-backend
-- ADR-0012).
CREATE TABLE consent_records (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id),
    document_code text NOT NULL,
    version text NOT NULL,
    granted boolean NOT NULL,
    recorded_at timestamptz NOT NULL,
    CONSTRAINT consent_records_agreement_fkey FOREIGN KEY (document_code, version)
        REFERENCES agreement_versions (document_code, version)
);

CREATE INDEX consent_records_user_idx ON consent_records (user_id);

-- Written and purged by the application (purge job, koala_app).
GRANT SELECT, INSERT, UPDATE, DELETE ON pending_registrations TO koala_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON password_reset_tokens TO koala_app;
-- Read-only reference data; a new version comes by migration.
GRANT SELECT ON agreement_versions TO koala_app;
-- Evidence: appended, never read back, updated or deleted.
GRANT INSERT ON consent_records TO koala_app;

-- +goose Down
DROP TABLE consent_records;
DROP TABLE agreement_versions;
DROP TABLE password_reset_tokens;
DROP TABLE pending_registrations;
ALTER TABLE user_sessions DROP CONSTRAINT user_sessions_revoke_reason_check;
ALTER TABLE user_sessions ADD CONSTRAINT user_sessions_revoke_reason_check CHECK (revoke_reason IN (
    'LOGOUT', 'REPLACED', 'USER_REVOKED', 'DEVICE_MISMATCH', 'REFRESH_REUSED', 'EXPIRED', 'INACTIVE'));
DROP VIEW active_users;
ALTER TABLE users
    DROP COLUMN first_name,
    DROP COLUMN last_name,
    DROP COLUMN employer,
    DROP COLUMN email_verified,
    DROP COLUMN phone_verified,
    DROP COLUMN identity_verified;
CREATE VIEW active_users WITH (security_barrier) AS
    SELECT * FROM users WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_users TO koala_app;
