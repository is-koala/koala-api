-- The Idempotency-Key store (issue #22, ADR-0034; plan §4.1-6, ADR-0005
-- madde 6; Stripe's model and draft-ietf-httpapi-idempotency-key-header).
--
-- One row per (actor, operation, key): the request it was first used with
-- and, once the request finished, its response. actor is the caller
-- ("user:<id>", "staff:<id>") or, for an operation without a caller, a
-- digest of the capability the request presents (completeRegistration:
-- the registration token's). request_hash is an HMAC-SHA256 of the
-- request's canonical form under a key derived from the login pepper, so
-- a leaked table does not tell which body a key was used with. response
-- is the stored status, headers and body sealed with AES-256-GCM under a
-- key derived from the request itself and the pepper: the database alone
-- cannot open it, only a replay of the same request can (ADR-0034 Karar 3).
--
-- state IN_PROGRESS holds the key for one attempt until locked_until; the
-- attempt id fences the request's own transaction, which writes the
-- response together with the business data (COMPLETED). A row whose
-- request failed with a 5xx or a transient 4xx is deleted, so the client
-- may retry. Rows are hard-deleted at expires_at, 24 hours after they were
-- written (M2).

-- +goose Up
CREATE TABLE idempotency_keys (
    actor text NOT NULL,
    operation text NOT NULL,
    key text NOT NULL,
    request_hash bytea NOT NULL,
    attempt uuid NOT NULL,
    state text NOT NULL,
    locked_until timestamptz NOT NULL,
    response bytea,
    created_at timestamptz NOT NULL,
    completed_at timestamptz,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (actor, operation, key),
    CONSTRAINT idempotency_keys_actor_check CHECK (length(actor) BETWEEN 1 AND 128),
    CONSTRAINT idempotency_keys_operation_check CHECK (operation ~ '^[a-z][A-Za-z0-9]{0,99}$'),
    CONSTRAINT idempotency_keys_key_check CHECK (key ~ '^[A-Za-z0-9_.:-]{8,255}$'),
    CONSTRAINT idempotency_keys_request_hash_check CHECK (octet_length(request_hash) = 32),
    CONSTRAINT idempotency_keys_state_check CHECK (state IN ('IN_PROGRESS', 'COMPLETED')),
    CONSTRAINT idempotency_keys_response_check CHECK (
        (state = 'COMPLETED') = (response IS NOT NULL)
        AND (state = 'COMPLETED') = (completed_at IS NOT NULL)),
    CONSTRAINT idempotency_keys_expiry_check CHECK (expires_at > created_at)
);

-- The purge goes by expiry.
CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

-- Claimed, completed, released and purged by the application (koala_app).
GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO koala_app;

-- +goose Down
DROP TABLE idempotency_keys;
