-- Marketplace authentication (ADR-0015, ADR-0025): the user's identity core,
-- devices, device-bound sessions, rotating refresh tokens and the login
-- attempt log.
--
-- users is the minimal identity core: id, login identifier (e-mail),
-- password hash and status. Profile, phone and TCKN come with Faz 2's
-- identity module. users and devices are soft-deleted (K14) and carry
-- personal data, so their views are security barriers (ADR-0017 §8).
--
-- Sessions, refresh tokens and login attempts are short-lived technical
-- rows (exemption class M2, ADR-0013): they are hard-deleted after their
-- TTL, never soft-deleted. koala_app reads and writes them directly; the
-- purge runs as koala_app too (a River periodic job).

-- +goose Up
CREATE TABLE users (
    id uuid PRIMARY KEY,
    -- The login identifier, trimmed and lower-cased by the application.
    email text NOT NULL,
    -- A PHC string ($argon2id$v=19$m=…,t=…,p=…$salt$hash). NULL: the user
    -- cannot sign in with a password (an OTP-only account of Faz 2).
    password_hash text,
    status text NOT NULL,
    created_at timestamptz NOT NULL,
    CONSTRAINT users_email_check CHECK (email = lower(btrim(email)) AND length(email) BETWEEN 3 AND 254),
    CONSTRAINT users_password_hash_check CHECK (password_hash IS NULL OR password_hash LIKE '$argon2id$%'),
    CONSTRAINT users_status_check CHECK (status IN ('ACTIVE', 'SUSPENDED'))
);
-- +koala soft-delete v1 users security_barrier
-- +koala begin generated (make generate); do not edit
ALTER TABLE users
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT users_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX users_deletion_id_idx ON users (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON users TO koala_maint;
CREATE VIEW active_users WITH (security_barrier) AS
    SELECT * FROM users WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_users TO koala_app;
-- +koala end generated

-- Business key among active rows (ADR-0013 §5).
CREATE UNIQUE INDEX users_email_key ON users (email) WHERE deleted_at IS NULL;

-- A device is one installation (mobile) or browser profile (web) of a user,
-- identified by the client's X-Device-Id (ADR-0015 §3, ADR-0021 §2).
CREATE TABLE devices (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_key text NOT NULL,
    -- X-Client-Platform of the sign-in; NULL when the client did not send it.
    platform text,
    created_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    CONSTRAINT devices_device_key_check CHECK (device_key ~ '^[A-Za-z0-9._:-]{8,128}$'),
    CONSTRAINT devices_platform_check CHECK (platform IN ('IOS', 'ANDROID', 'WEB'))
);
-- +koala soft-delete v1 devices security_barrier
-- +koala begin generated (make generate); do not edit
ALTER TABLE devices
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT devices_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX devices_deletion_id_idx ON devices (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON devices TO koala_maint;
CREATE VIEW active_devices WITH (security_barrier) AS
    SELECT * FROM devices WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_devices TO koala_app;
-- +koala end generated

-- One active row per (user, device key); it also serves the cascade edge
-- devices.user_id → users.
CREATE UNIQUE INDEX devices_user_device_key ON devices (user_id, device_key) WHERE deleted_at IS NULL;

-- One device-bound session. The session is the refresh token family: reuse
-- of a rotated refresh token revokes it (K12, ADR-0025).
CREATE TABLE user_sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id uuid NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    -- Absolute end of the session; no refresh extends it.
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    revoke_reason text,
    CONSTRAINT user_sessions_expiry_check CHECK (expires_at > created_at),
    CONSTRAINT user_sessions_revoked_together CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL)),
    CONSTRAINT user_sessions_revoke_reason_check CHECK (revoke_reason IN (
        'LOGOUT', 'REPLACED', 'USER_REVOKED', 'DEVICE_MISMATCH', 'REFRESH_REUSED', 'EXPIRED', 'INACTIVE'))
);

CREATE INDEX user_sessions_user_open_idx ON user_sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX user_sessions_device_open_idx ON user_sessions (device_id) WHERE revoked_at IS NULL;
CREATE INDEX user_sessions_expires_at_idx ON user_sessions (expires_at);

-- Opaque refresh tokens, stored as SHA-256 (32 bytes of entropy make a
-- slow hash unnecessary, plan §3.9). Each refresh consumes the presented
-- token and issues a new one; a consumed token presented again is reuse.
CREATE TABLE user_refresh_tokens (
    id uuid PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES user_sessions (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL,
    created_at timestamptz NOT NULL,
    -- Sliding (idle) limit of this token, never past the session's end.
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    CONSTRAINT user_refresh_tokens_hash_check CHECK (octet_length(token_hash) = 32),
    CONSTRAINT user_refresh_tokens_expiry_check CHECK (expires_at > created_at),
    CONSTRAINT user_refresh_tokens_hash_key UNIQUE (token_hash)
);

CREATE INDEX user_refresh_tokens_session_idx ON user_refresh_tokens (session_id);

-- Every sign-in attempt, failed or not, for both principal kinds. The
-- target is the SHA-256 of the normalized identifier, so an attempt on an
-- address that has no account stores no address.
CREATE TABLE login_attempts (
    id uuid PRIMARY KEY,
    principal_kind text NOT NULL,
    target_hash bytea NOT NULL,
    succeeded boolean NOT NULL,
    created_at timestamptz NOT NULL,
    CONSTRAINT login_attempts_principal_kind_check CHECK (principal_kind IN ('USER', 'STAFF')),
    CONSTRAINT login_attempts_target_hash_check CHECK (octet_length(target_hash) = 32)
);

CREATE INDEX login_attempts_target_idx ON login_attempts (target_hash, created_at);
CREATE INDEX login_attempts_created_at_idx ON login_attempts (created_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON user_sessions, user_refresh_tokens, login_attempts TO koala_app;

-- +goose Down
DROP TABLE login_attempts;
DROP TABLE user_refresh_tokens;
DROP TABLE user_sessions;
DROP VIEW active_devices;
DROP TABLE devices;
DROP VIEW active_users;
DROP TABLE users;
