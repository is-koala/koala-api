-- name: RecordIntent :exec
-- RecordIntent writes the user's open profile sync intent in the caller's
-- transaction, or refreshes the open one: its time and the origin of the
-- latest change (USR-022). No payload: the dispatcher reads the profile
-- when it publishes.
INSERT INTO user_profile_sync_intents (id, user_id, created_at, updated_at, request_id, session_id, device_key)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(now), sqlc.arg(now), sqlc.narg(request_id), sqlc.narg(session_id),
    sqlc.narg(device_key))
ON CONFLICT (user_id) WHERE published_at IS NULL
DO UPDATE SET updated_at = EXCLUDED.updated_at, request_id = EXCLUDED.request_id,
    session_id = EXCLUDED.session_id, device_key = EXCLUDED.device_key;
