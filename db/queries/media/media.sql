-- name: LockUploader :exec
-- LockUploader serializes one user's upload starts until the transaction
-- ends: the open-upload cap and the daily budget are counted and then
-- spent, so two concurrent starts cannot both pass them (ADR-0035 Karar
-- 6). The two-key form keeps the class apart from every other advisory
-- lock.
SELECT pg_advisory_xact_lock(sqlc.arg(lock_class)::integer, hashtext(sqlc.arg(owner_id)::uuid::text));

-- name: CountOpenUploads :one
-- CountOpenUploads counts the owner's uploads that are still PENDING.
SELECT count(*)
FROM active_media
WHERE owner_id = sqlc.arg(owner_id)::uuid AND status = 'PENDING';

-- name: CountUploadStarts :one
-- CountUploadStarts counts the user's starts after since.
SELECT count(*)
FROM media_upload_starts
WHERE user_id = sqlc.arg(user_id)::uuid AND created_at > sqlc.arg(since)::timestamptz;

-- name: OldestUploadStart :one
-- OldestUploadStart is the user's oldest start after since: the window
-- frees a place when that one leaves it.
SELECT created_at
FROM media_upload_starts
WHERE user_id = sqlc.arg(user_id)::uuid AND created_at > sqlc.arg(since)::timestamptz
ORDER BY created_at
LIMIT 1;

-- name: InsertUploadStart :exec
INSERT INTO media_upload_starts (id, user_id, created_at)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(user_id)::uuid, sqlc.arg(created_at)::timestamptz);

-- name: PurgeUploadStarts :execrows
-- PurgeUploadStarts hard-deletes the ledger rows written before the bound
-- (M2, a day).
DELETE FROM media_upload_starts WHERE created_at < $1;

-- name: InsertMedia :one
INSERT INTO active_media (id, owner_id, kind, status, upload_key, declared_size, created_at)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(owner_id)::uuid, sqlc.arg(kind)::text, 'PENDING',
    sqlc.arg(upload_key)::text, sqlc.arg(declared_size)::bigint, sqlc.arg(created_at)::timestamptz)
RETURNING id, owner_id, kind, status, upload_key, declared_size, created_at, completion_requested_at,
    decided_at, object_key, content_type, size_bytes, rejection;

-- name: GetOwnMedia :one
-- GetOwnMedia reads one of the owner's media. Another user's media and
-- one that never existed are the same no row (ADR-0014).
SELECT id, owner_id, kind, status, upload_key, declared_size, created_at, completion_requested_at,
    decided_at, object_key, content_type, size_bytes, rejection
FROM active_media
WHERE id = sqlc.arg(id)::uuid AND owner_id = sqlc.arg(owner_id)::uuid;

-- name: LockOwnMedia :one
-- LockOwnMedia locks one of the owner's media for the transaction: the
-- completion gate and the deletion decide under it.
SELECT id, owner_id, kind, status, upload_key, declared_size, created_at, completion_requested_at,
    decided_at, object_key, content_type, size_bytes, rejection
FROM active_media
WHERE id = sqlc.arg(id)::uuid AND owner_id = sqlc.arg(owner_id)::uuid
FOR UPDATE;

-- name: LockMedia :one
-- LockMedia locks a media row by id for the completion job, which has no
-- caller.
SELECT id, owner_id, kind, status, upload_key, declared_size, created_at, completion_requested_at,
    decided_at, object_key, content_type, size_bytes, rejection
FROM active_media
WHERE id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: RequestCompletion :one
-- RequestCompletion records the first request to complete a PENDING
-- upload; a later request keeps the first time.
UPDATE active_media
SET completion_requested_at = COALESCE(completion_requested_at, sqlc.arg(now)::timestamptz)
WHERE id = sqlc.arg(id)::uuid AND status = 'PENDING'
RETURNING id, owner_id, kind, status, upload_key, declared_size, created_at, completion_requested_at,
    decided_at, object_key, content_type, size_bytes, rejection;

-- name: MarkReady :execrows
-- MarkReady is PENDING → READY. The status condition is the state
-- machine's last word: a row that left PENDING is not written.
UPDATE active_media
SET status = 'READY', decided_at = sqlc.arg(now)::timestamptz, object_key = sqlc.arg(object_key)::text,
    content_type = sqlc.arg(content_type)::text, size_bytes = sqlc.arg(size_bytes)::bigint
WHERE id = sqlc.arg(id)::uuid AND status = 'PENDING';

-- name: MarkRejected :execrows
-- MarkRejected is PENDING → REJECTED.
UPDATE active_media
SET status = 'REJECTED', decided_at = sqlc.arg(now)::timestamptz, rejection = sqlc.arg(rejection)::text
WHERE id = sqlc.arg(id)::uuid AND status = 'PENDING';

-- name: ListSweepable :many
-- ListSweepable pages, by (created_at, id) after the cursor, through the
-- media the sweep removes (ADR-0035 Karar 4 and 8):
--   - an upload that did not end READY and was started before
--     unfinished_before, unless its completion was asked for after
--     requested_before (a job may be writing its object right now);
--   - a READY media nothing bound, decided before unbound_before.
-- No lock: retire locks and checks each row again.
SELECT id, kind, upload_key, created_at
FROM active_media
WHERE (created_at, id) > (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid)
    AND (
        (status IN ('PENDING', 'REJECTED') AND created_at < sqlc.arg(unfinished_before)::timestamptz
            AND (completion_requested_at IS NULL OR completion_requested_at < sqlc.arg(requested_before)::timestamptz))
        OR (status = 'READY' AND bound_at IS NULL AND decided_at < sqlc.arg(unbound_before)::timestamptz))
ORDER BY created_at, id
LIMIT sqlc.arg(batch)::integer;

-- name: LockSweepable :one
-- LockSweepable locks one media ListSweepable returned if it is still to
-- be swept; a row a completion holds is skipped (SKIP LOCKED) and seen
-- again next run.
SELECT status
FROM active_media
WHERE id = sqlc.arg(id)::uuid
    AND (
        (status IN ('PENDING', 'REJECTED') AND created_at < sqlc.arg(unfinished_before)::timestamptz
            AND (completion_requested_at IS NULL OR completion_requested_at < sqlc.arg(requested_before)::timestamptz))
        OR (status = 'READY' AND bound_at IS NULL AND decided_at < sqlc.arg(unbound_before)::timestamptz))
FOR UPDATE SKIP LOCKED;

-- name: OldestOpenUpload :one
-- OldestOpenUpload is when the owner's oldest PENDING upload started: the
-- sweep frees its place UploadRetention later at the latest.
SELECT created_at
FROM active_media
WHERE owner_id = sqlc.arg(owner_id)::uuid AND status = 'PENDING'
ORDER BY created_at
LIMIT 1;

-- name: BindMedia :one
-- BindMedia marks one of the owner's READY media bound, once; it is the
-- one writer of bound_at (ADR-0035 Karar 8). The update takes the row's
-- lock, which a concurrent soft deletion conflicts with, so it stands in
-- for softdelete.LockActiveParent. No row: the media is not the owner's,
-- not there, or not READY; media.Bind tells them apart.
UPDATE active_media
SET bound_at = COALESCE(bound_at, sqlc.arg(now)::timestamptz)
WHERE id = sqlc.arg(id)::uuid AND owner_id = sqlc.arg(owner_id)::uuid AND status = 'READY'
RETURNING id;

-- name: ActiveObject :one
-- ActiveObject is the status and final key of the active row with the id,
-- if there is one: the object deletion job keeps what an active row
-- names.
SELECT status, object_key FROM active_media WHERE id = sqlc.arg(id)::uuid;
