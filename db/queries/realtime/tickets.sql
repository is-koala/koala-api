-- name: InsertTicket :exec
INSERT INTO realtime_tickets (token_hash, session_id, user_id, device_key, issued_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: TakeTicket :one
-- TakeTicket redeems a ticket: the row is deleted and returned in one
-- statement, so of two concurrent redemptions at most one gets it
-- (ADR-0018 §2).
DELETE FROM realtime_tickets
WHERE token_hash = $1
RETURNING session_id, user_id, device_key, issued_at, expires_at;

-- name: PurgeTickets :execrows
DELETE FROM realtime_tickets WHERE expires_at < $1;
