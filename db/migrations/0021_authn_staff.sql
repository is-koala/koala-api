-- Staff authentication (ADR-0015 §5, ADR-0025): staff accounts with lockout,
-- must_change_password and token_version, and staff sessions with rotating
-- refresh tokens. The staff surface is a separate token class; staff roles
-- and permissions (RBAC) come with migrations 0040+.
--
-- staff_members is soft-deleted (K14) and holds personal data (security
-- barrier view). Staff sessions and refresh tokens are M2: hard-deleted
-- after their TTL.
--
-- TOTP (K13) lands in Faz 8. The session already records when its second
-- factor was verified (mfa_verified_at); until then it stays NULL and the
-- staff access token's amr claim says "pwd" only.

-- +goose Up
CREATE TABLE staff_members (
    id uuid PRIMARY KEY,
    email text NOT NULL,
    password_hash text NOT NULL,
    status text NOT NULL,
    must_change_password boolean NOT NULL,
    -- Consecutive wrong passwords on the account, from any address, since
    -- the last success. From StaffBackoffAfter on, each failure sets an
    -- exponentially growing backoff_until (at most 15 minutes) during
    -- which every attempt is refused unexamined. There is no hard lock
    -- on the account: an attacker cannot keep a member out from other
    -- addresses (ADR-0025 §6).
    failed_login_attempts integer NOT NULL DEFAULT 0,
    backoff_until timestamptz,
    -- Bumped by logout, password change and disabling: every access and
    -- refresh token issued before carries an older version and dies.
    token_version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL,
    password_changed_at timestamptz,
    CONSTRAINT staff_members_email_check CHECK (email = lower(btrim(email)) AND length(email) BETWEEN 3 AND 254),
    CONSTRAINT staff_members_password_hash_check CHECK (password_hash LIKE '$argon2id$%'),
    CONSTRAINT staff_members_status_check CHECK (status IN ('ACTIVE', 'DISABLED')),
    CONSTRAINT staff_members_failed_login_attempts_check CHECK (failed_login_attempts >= 0),
    CONSTRAINT staff_members_token_version_check CHECK (token_version > 0)
);
-- +koala soft-delete v1 staff_members security_barrier
-- +koala begin generated (make generate); do not edit
ALTER TABLE staff_members
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT staff_members_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX staff_members_deletion_id_idx ON staff_members (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON staff_members TO koala_maint;
CREATE VIEW active_staff_members WITH (security_barrier) AS
    SELECT * FROM staff_members WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_staff_members TO koala_app;
-- +koala end generated

-- Staff accounts are created by the bootstrap and staff management
-- (koala_maint, Faz 8), not by the application role.
REVOKE INSERT ON active_staff_members FROM koala_app;

CREATE UNIQUE INDEX staff_members_email_key ON staff_members (email) WHERE deleted_at IS NULL;

-- The lock of one (account, client address) pair (PCI DSS 4.0 8.3.4):
-- StaffLockThreshold consecutive wrong passwords from the address lock
-- the account for that address for StaffLockDuration. Other addresses
-- are unaffected.
CREATE TABLE staff_login_locks (
    staff_id uuid NOT NULL REFERENCES staff_members (id) ON DELETE CASCADE,
    -- The client address; an IPv6 client by its /64.
    client_key text NOT NULL,
    failures integer NOT NULL,
    locked_until timestamptz,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (staff_id, client_key),
    CONSTRAINT staff_login_locks_failures_check CHECK (failures >= 0),
    CONSTRAINT staff_login_locks_client_key_check CHECK (length(client_key) BETWEEN 1 AND 64)
);

CREATE INDEX staff_login_locks_updated_at_idx ON staff_login_locks (updated_at);

-- A staff sign-in. The staff surface sends no device id (ADR-0021 §2);
-- the session is bound to the account's token_version instead.
CREATE TABLE staff_sessions (
    id uuid PRIMARY KEY,
    staff_id uuid NOT NULL REFERENCES staff_members (id) ON DELETE CASCADE,
    token_version bigint NOT NULL,
    created_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    mfa_verified_at timestamptz,
    revoked_at timestamptz,
    revoke_reason text,
    CONSTRAINT staff_sessions_expiry_check CHECK (expires_at > created_at),
    CONSTRAINT staff_sessions_revoked_together CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL)),
    CONSTRAINT staff_sessions_revoke_reason_check CHECK (revoke_reason IN ('LOGOUT', 'REFRESH_REUSED', 'EXPIRED', 'INACTIVE', 'LOCKED'))
);

CREATE INDEX staff_sessions_staff_open_idx ON staff_sessions (staff_id) WHERE revoked_at IS NULL;
CREATE INDEX staff_sessions_expires_at_idx ON staff_sessions (expires_at);

CREATE TABLE staff_refresh_tokens (
    id uuid PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES staff_sessions (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    successor_salt bytea,
    CONSTRAINT staff_refresh_tokens_hash_check CHECK (octet_length(token_hash) = 32),
    CONSTRAINT staff_refresh_tokens_expiry_check CHECK (expires_at > created_at),
    CONSTRAINT staff_refresh_tokens_consumed_together CHECK ((consumed_at IS NULL) = (successor_salt IS NULL)),
    CONSTRAINT staff_refresh_tokens_hash_key UNIQUE (token_hash)
);

CREATE INDEX staff_refresh_tokens_session_idx ON staff_refresh_tokens (session_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON staff_sessions, staff_login_locks TO koala_app;
GRANT SELECT, INSERT, UPDATE ON staff_refresh_tokens TO koala_app;

-- +goose Down
DROP TABLE staff_refresh_tokens;
DROP TABLE staff_sessions;
DROP TABLE staff_login_locks;
DROP VIEW active_staff_members;
DROP TABLE staff_members;
