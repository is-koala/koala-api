-- E-mail and phone verification of a signed-in user (ADR-0029 Karar 3,
-- ADR-0033).
--
-- users gains phone: the verified number in E.164, NULL until one is
-- confirmed. Like users.email it is written only by a confirmed
-- verification (USR-014), and like it at most one active account holds a
-- number. phone_verified (0042) is its flag.
--
-- user_verifications: short-lived (M2). One row is one code sent to one
-- target for one user. The code is six decimal digits over the whole
-- space, minted by the job that sends it (never in the request, never in
-- the job's arguments) and stored only as a keyed digest: HMAC-SHA256
-- under a key derived from the login pepper, over the row id and the
-- code. A plain SHA-256 of a six-digit code is reversed by trying all
-- million values, so the digest is keyed: a leaked table alone yields no
-- code. The digest is cleared the moment the verification ends.
-- target_key is the HMAC of the budget key (the mailbox for an address,
-- the E.164 number for a phone) under the login pepper: the resend budget
-- counts targets, not spellings (ADR-0032 Karar 6's scheme). Rows are
-- hard-deleted two days after they were written: the longest budget
-- window is a day.

-- +goose Up
DROP VIEW active_users;
ALTER TABLE users
    ADD COLUMN phone text,
    ADD CONSTRAINT users_phone_check CHECK (phone ~ '^\+[1-9][0-9]{7,14}$');
-- +koala soft-delete-view v1 users security_barrier
-- +koala begin generated (make generate); do not edit
CREATE VIEW active_users WITH (security_barrier) AS
    SELECT * FROM users WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_users TO koala_app;
-- +koala end generated

-- Business key among active rows (ADR-0013 §5), like users_email_key.
CREATE UNIQUE INDEX users_phone_key ON users (phone) WHERE deleted_at IS NULL;

CREATE TABLE user_verifications (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel text NOT NULL,
    -- The new address (trimmed and lower-cased, like users.email) or
    -- number (E.164). Which numbers are accepted is the application's
    -- policy (ADR-0033); the column only holds the form.
    target text NOT NULL,
    target_key bytea NOT NULL,
    created_at timestamptz NOT NULL,
    -- Ten minutes from created_at (NIST SP 800-63B Rev. 3 §5.1.3.2).
    expires_at timestamptz NOT NULL,
    -- NULL until the job sent the code, and again once the verification
    -- ended. Each send mints a new code and replaces the digest.
    code_digest bytea,
    code_sent_at timestamptz,
    failed_attempts integer NOT NULL,
    confirmed_at timestamptz,
    canceled_at timestamptz,
    -- The fifth wrong code burns the verification (USR-009).
    burned_at timestamptz,
    CONSTRAINT user_verifications_channel_check CHECK (channel IN ('EMAIL', 'PHONE')),
    CONSTRAINT user_verifications_target_check CHECK (
        (channel = 'EMAIL' AND target = lower(btrim(target)) AND length(target) BETWEEN 3 AND 254)
        OR (channel = 'PHONE' AND target ~ '^\+[1-9][0-9]{7,14}$')),
    CONSTRAINT user_verifications_target_key_check CHECK (octet_length(target_key) = 32),
    CONSTRAINT user_verifications_expiry_check CHECK (expires_at > created_at),
    CONSTRAINT user_verifications_digest_check CHECK (
        (code_digest IS NULL OR octet_length(code_digest) = 32)
        AND (code_digest IS NULL OR code_sent_at IS NOT NULL)),
    -- At most five wrong codes, and the fifth is the one that burns: the
    -- limit holds in the database too, whatever the code does.
    CONSTRAINT user_verifications_attempts_check CHECK (failed_attempts BETWEEN 0 AND 5),
    CONSTRAINT user_verifications_burned_check CHECK ((burned_at IS NOT NULL) = (failed_attempts = 5)),
    CONSTRAINT user_verifications_ended_check CHECK (
        num_nonnulls(confirmed_at, canceled_at, burned_at) <= 1
        AND (num_nonnulls(confirmed_at, canceled_at, burned_at) = 0 OR code_digest IS NULL))
);

-- One open verification per (user, channel, target); a new start cancels
-- the open one first (USR-010).
CREATE UNIQUE INDEX user_verifications_open_key ON user_verifications (user_id, channel, target)
    WHERE confirmed_at IS NULL AND canceled_at IS NULL AND burned_at IS NULL;
-- The target's resend budget, over every user.
CREATE INDEX user_verifications_target_created_idx ON user_verifications (target_key, created_at);
-- The caller's list and its daily cap per channel.
CREATE INDEX user_verifications_user_created_idx ON user_verifications (user_id, created_at);
-- The global hourly cap of text messages.
CREATE INDEX user_verifications_sms_sent_idx ON user_verifications (code_sent_at) WHERE channel = 'PHONE';
CREATE INDEX user_verifications_created_at_idx ON user_verifications (created_at);

-- Written and purged by the application (purge job, koala_app).
GRANT SELECT, INSERT, UPDATE, DELETE ON user_verifications TO koala_app;

-- +goose Down
DROP TABLE user_verifications;
DROP VIEW active_users;
ALTER TABLE users DROP COLUMN phone;
CREATE VIEW active_users WITH (security_barrier) AS
    SELECT * FROM users WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_users TO koala_app;
