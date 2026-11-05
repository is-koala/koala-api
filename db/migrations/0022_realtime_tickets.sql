-- WebSocket tickets (ADR-0018 §2, ADR-0022, ADR-0025): issued to an
-- authenticated device session, redeemed once by
-- DELETE … WHERE token_hash = $1 RETURNING, so redemption stays single-use
-- across instances. Only the SHA-256 of the ticket is stored. Exemption
-- class M2: rows live 60 seconds; expired rows are purged.

-- +goose Up
CREATE TABLE realtime_tickets (
    token_hash bytea PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES user_sessions (id) ON DELETE CASCADE,
    user_id uuid NOT NULL,
    device_key text NOT NULL,
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    CONSTRAINT realtime_tickets_hash_check CHECK (octet_length(token_hash) = 32),
    CONSTRAINT realtime_tickets_expiry_check CHECK (expires_at > issued_at)
);

CREATE INDEX realtime_tickets_session_idx ON realtime_tickets (session_id);
CREATE INDEX realtime_tickets_expires_at_idx ON realtime_tickets (expires_at);

GRANT SELECT, INSERT, DELETE ON realtime_tickets TO koala_app;

-- +goose Down
DROP TABLE realtime_tickets;
