-- Three-step registration, password reset and the in-app password change
-- (ADR-0029 Karar 2 and 4, ADR-0032).
--
-- users gains the registration fields. phone_verified and
-- identity_verified keep their default: a new account is never verified
-- by phone or identity document (USR-006); the other columns lose theirs,
-- so every insert states them. password_changed_at is the account's
-- credential epoch: a password reset or change moves it, and only a
-- session opened after it vouches for its device (ADR-0032 Karar 7).
-- devices gains credentials_proven_at: when a password reset was completed
-- from the device, which proved the mailbox there (ADR-0032 Karar 7).
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
    ADD COLUMN identity_verified boolean NOT NULL DEFAULT false,
    ADD COLUMN password_changed_at timestamptz;
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

DROP VIEW active_devices;
ALTER TABLE devices ADD COLUMN credentials_proven_at timestamptz;
-- +koala soft-delete-view v1 devices security_barrier
-- +koala begin generated (make generate); do not edit
CREATE VIEW active_devices WITH (security_barrier) AS
    SELECT * FROM devices WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_devices TO koala_app;
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
-- At most one open registration per address; a new start revokes the open
-- one. mailbox_key is the HMAC-SHA256, under the login pepper, of the
-- address without its sub-address and, at Gmail, without dots: the mail
-- budget counts mailboxes, not spellings, and only compares keys, so no
-- address is stored for it (ADR-0032 Karar 6 and 8; the login_attempts
-- target's scheme, ADR-0025 §5). Once a registration ends (consumed or
-- revoked) its address and names are cleared at once; only the keyed
-- mailbox and the times stay until the row is hard-deleted 30 days after
-- it was written (USR-007; KVKK data minimization).
CREATE TABLE pending_registrations (
    id uuid PRIMARY KEY,
    -- Trimmed and lower-cased by the application, like users.email.
    email text,
    mailbox_key bytea NOT NULL,
    first_name text,
    last_name text,
    employer boolean NOT NULL,
    created_at timestamptz NOT NULL,
    link_token_hash bytea,
    link_expires_at timestamptz,
    registration_token_hash bytea,
    registration_expires_at timestamptz,
    consumed_at timestamptz,
    revoked_at timestamptz,
    CONSTRAINT pending_registrations_email_check CHECK (email = lower(btrim(email)) AND length(email) BETWEEN 3 AND 254),
    CONSTRAINT pending_registrations_mailbox_key_check CHECK (octet_length(mailbox_key) = 32),
    CONSTRAINT pending_registrations_names_check CHECK (
        length(first_name) BETWEEN 1 AND 100 AND length(last_name) BETWEEN 1 AND 100),
    -- An open registration has its address and names; an ended one has
    -- none of them.
    CONSTRAINT pending_registrations_personal_check CHECK (
        (consumed_at IS NULL AND revoked_at IS NULL)
            = (email IS NOT NULL AND first_name IS NOT NULL AND last_name IS NOT NULL)
        AND (email IS NULL) = (first_name IS NULL) AND (first_name IS NULL) = (last_name IS NULL)),
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
-- The mail budget counts a mailbox's recent rows; the purge goes by time.
CREATE INDEX pending_registrations_mailbox_created_idx ON pending_registrations (mailbox_key, created_at);
CREATE INDEX pending_registrations_created_at_idx ON pending_registrations (created_at);

-- One row per registration request the job handles, written before the
-- address is looked up: the global hourly cap counts these rows, so a
-- registered address spends the cap exactly like an unknown one and the
-- cap tells nothing about which addresses exist (ADR-0032 Karar 2 and 6).
-- Only the time is kept; rows are hard-deleted a day after (M2).
CREATE TABLE registration_requests (
    id uuid PRIMARY KEY,
    requested_at timestamptz NOT NULL
);

CREATE INDEX registration_requests_requested_at_idx ON registration_requests (requested_at);

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
-- consents given to it keep their reference. locale, content_url and
-- content_hash name the text a consent was given to: the evidence of what
-- was agreed. Seeded with Java's versions and addresses (koala-backend
-- 2026-08-27-13-seed-agreement-versions, 2026-09-05-05). Java's seed
-- carries no content hash; it stays NULL until the texts are hashed.
CREATE TABLE agreement_versions (
    document_code text NOT NULL,
    version text NOT NULL,
    locale text NOT NULL,
    content_url text,
    content_hash text,
    mandatory boolean NOT NULL,
    effective_from timestamptz NOT NULL,
    retired_at timestamptz,
    PRIMARY KEY (document_code, version),
    CONSTRAINT agreement_versions_document_code_check CHECK (document_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    CONSTRAINT agreement_versions_version_check CHECK (length(version) BETWEEN 1 AND 32),
    CONSTRAINT agreement_versions_locale_check CHECK (locale ~ '^[a-z]{2}(-[A-Z]{2})?$'),
    CONSTRAINT agreement_versions_content_url_check CHECK (content_url IS NULL OR content_url LIKE 'https://%'),
    CONSTRAINT agreement_versions_content_hash_check CHECK (content_hash IS NULL OR length(content_hash) BETWEEN 1 AND 128)
);

CREATE UNIQUE INDEX agreement_versions_current_key ON agreement_versions (document_code)
    WHERE retired_at IS NULL;

-- DISTANCE_SERVICE_CONTRACT is not a registration gate: Java asks for it
-- with a transaction (2026-09-05-05).
INSERT INTO agreement_versions (document_code, version, locale, content_url, mandatory, effective_from) VALUES
    ('TERMS_OF_SERVICE', '1.0', 'tr', 'https://iskoala.com/kullanim-kosullari/v/1.0', true, '2026-08-27T00:00:00Z'),
    ('PRIVACY_POLICY', '1.0', 'tr', 'https://iskoala.com/gizlilik-politikasi/v/1.0', true, '2026-08-27T00:00:00Z'),
    ('EXPLICIT_CONSENT', '1.0', 'tr', 'https://iskoala.com/kvkk-aydinlatma/v/1.0#acik-riza', true, '2026-08-27T00:00:00Z'),
    ('COMMERCIAL_MESSAGE', '1.0', 'tr', 'https://iskoala.com/kvkk-aydinlatma/v/1.0#ticari-ileti', false, '2026-08-27T00:00:00Z'),
    ('DISTANCE_SERVICE_CONTRACT', '1.0', 'tr', 'https://iskoala.com/mesafeli-hizmet-sozlesmesi/v/1.0', false, '2026-09-05T00:00:00Z');

-- One consent decision of a user, given at registration: evidence, never
-- updated or deleted. The account's anonymization keeps it (koala-backend
-- ADR-0012). The client address (as the edge resolved it behind the
-- trusted proxies) and the User-Agent are kept as Java keeps them; they are
-- to be anonymized three years after the consent ends (ADR-0032 Karar 8,
-- legal confirmation pending), a job that comes with the PR that models a
-- consent's end.
CREATE TABLE consent_records (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id),
    document_code text NOT NULL,
    version text NOT NULL,
    granted boolean NOT NULL,
    recorded_at timestamptz NOT NULL,
    ip_address text,
    user_agent text,
    CONSTRAINT consent_records_ip_address_check CHECK (ip_address IS NULL OR length(ip_address) BETWEEN 2 AND 45),
    CONSTRAINT consent_records_user_agent_check CHECK (user_agent IS NULL OR octet_length(user_agent) <= 512),
    CONSTRAINT consent_records_agreement_fkey FOREIGN KEY (document_code, version)
        REFERENCES agreement_versions (document_code, version)
);

CREATE INDEX consent_records_user_idx ON consent_records (user_id);

-- Written and purged by the application (purge job, koala_app).
GRANT SELECT, INSERT, UPDATE, DELETE ON pending_registrations TO koala_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON password_reset_tokens TO koala_app;
-- The cap's ledger: appended, counted and purged, never updated.
GRANT SELECT, INSERT, DELETE ON registration_requests TO koala_app;
-- Read-only reference data; a new version comes by migration.
GRANT SELECT ON agreement_versions TO koala_app;
-- Evidence: appended, never read back, updated or deleted.
GRANT INSERT ON consent_records TO koala_app;

-- +goose Down
DROP TABLE consent_records;
DROP TABLE agreement_versions;
DROP TABLE password_reset_tokens;
DROP TABLE registration_requests;
DROP TABLE pending_registrations;
ALTER TABLE user_sessions DROP CONSTRAINT user_sessions_revoke_reason_check;
ALTER TABLE user_sessions ADD CONSTRAINT user_sessions_revoke_reason_check CHECK (revoke_reason IN (
    'LOGOUT', 'REPLACED', 'USER_REVOKED', 'DEVICE_MISMATCH', 'REFRESH_REUSED', 'EXPIRED', 'INACTIVE'));
DROP VIEW active_devices;
ALTER TABLE devices DROP COLUMN credentials_proven_at;
CREATE VIEW active_devices WITH (security_barrier) AS
    SELECT * FROM devices WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_devices TO koala_app;
DROP VIEW active_users;
ALTER TABLE users
    DROP COLUMN password_changed_at,
    DROP COLUMN first_name,
    DROP COLUMN last_name,
    DROP COLUMN employer,
    DROP COLUMN email_verified,
    DROP COLUMN phone_verified,
    DROP COLUMN identity_verified;
CREATE VIEW active_users WITH (security_barrier) AS
    SELECT * FROM users WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_users TO koala_app;
