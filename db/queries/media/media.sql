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

-- name: LockUnfinished :many
-- LockUnfinished locks a batch of uploads that did not end READY and
-- were started before the bound, oldest first. A row another transaction
-- holds (the completion job deciding it) is skipped and seen next run.
SELECT id, owner_id, kind, status, upload_key, declared_size, created_at, completion_requested_at,
    decided_at, object_key, content_type, size_bytes, rejection
FROM active_media
WHERE status IN ('PENDING', 'REJECTED') AND created_at < sqlc.arg(before)::timestamptz
ORDER BY created_at
LIMIT sqlc.arg(batch)::integer
FOR UPDATE SKIP LOCKED;

-- name: MediaExists :one
-- MediaExists reports whether an active row has the id: a restored row
-- keeps its objects.
SELECT EXISTS (SELECT FROM active_media WHERE id = sqlc.arg(id)::uuid);
