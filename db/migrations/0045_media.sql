-- Media uploads (ADR-0029 Karar 6, ADR-0030, ADR-0035).
--
-- media: one upload of one user, from the start (PENDING, a quarantine key
-- and a write address) through the River job that validates, strips and
-- moves the object (READY with its final key, or REJECTED with why).
-- Soft-deleted (K14) and personal data: an identity document photo, a
-- profile picture and a CV all belong to a person, so the view is a
-- security barrier (ADR-0017 §8). A user's media go with the user
-- (cascade edge, db/softdelete.yaml).
--
-- The identifier of both keys is the row's id (ADR-0035 Karar 4): the
-- quarantine key is incoming/<id>.<ext> and the final key is
-- <prefix>/<id>.<ext>. The two CHECKs below hold it in the database too.
-- It is what makes every object the pipeline can leave behind reachable
-- from a row: the final keys an id can have are its kind's prefix times
-- the extensions the kind accepts, and the orphan sweep deletes the ones
-- the row does not name. media_object_key_check also ties the prefix to
-- the kind and the extension to the detected type.
--
-- rejection is the pipeline's own reason (internal/media.Rejection), four
-- values; the contract renders three codes and folds CORRUPT into
-- MEDIA_TYPE_UNSUPPORTED (ADR-0035 Karar 3). The row keeps the distinction
-- the response does not.
--
-- media_upload_starts: the daily upload budget's ledger (ADR-0035 Karar 6),
-- one row per started upload with its user and time. M2, hard-deleted a
-- day after. It is a ledger of its own and not a count of media rows on
-- purpose: deleting a media row (soft) hides it from active_media, so a
-- budget that counted media could be refilled by deleting uploads.

-- +goose Up
CREATE TABLE media (
    id uuid PRIMARY KEY,
    owner_id uuid NOT NULL REFERENCES users (id),
    kind text NOT NULL,
    status text NOT NULL,
    -- The quarantine key the write address was signed for.
    upload_key text NOT NULL,
    -- The size the client declared at the start. It bounds nothing in the
    -- store (ADR-0030 Karar 1); it is kept as evidence.
    declared_size bigint NOT NULL,
    created_at timestamptz NOT NULL,
    -- When the owner asked for the completion; the job is queued with it.
    completion_requested_at timestamptz,
    -- When the job decided: READY or REJECTED.
    decided_at timestamptz,
    -- READY only: the final key, the type detected in the bytes and the
    -- stored size after the strip.
    object_key text,
    content_type text,
    size_bytes bigint,
    -- REJECTED only.
    rejection text,
    -- When a consuming resource (a profile, a CV slot, a company gallery,
    -- an identity review) bound this READY media; NULL while nothing
    -- refers to it. Written only by media.Bind, in the binding
    -- transaction. A READY media unbound for seven days is swept
    -- (ADR-0035 Karar 8).
    bound_at timestamptz,
    CONSTRAINT media_kind_check CHECK (
        kind IN ('PROFILE_PICTURE', 'COMPANY_LOGO', 'COMPANY_IMAGE', 'IDENTITY_DOCUMENT', 'CV')),
    CONSTRAINT media_status_check CHECK (status IN ('PENDING', 'READY', 'REJECTED')),
    CONSTRAINT media_upload_key_check CHECK (
        upload_key ~ ('^incoming/' || id::text || '\.(jpg|png|pdf|doc|docx)$')),
    -- The final key is the kind's prefix, the row's id and the detected
    -- type's extension (internal/media.Kind.Prefix, ExtOf): the database
    -- holds the same mapping, so a row cannot name another kind's prefix
    -- or an extension its type does not have.
    CONSTRAINT media_object_key_check CHECK (
        object_key IS NULL
        OR object_key = (
            CASE kind
                WHEN 'PROFILE_PICTURE' THEN 'images/'
                WHEN 'COMPANY_LOGO' THEN 'images/'
                WHEN 'COMPANY_IMAGE' THEN 'images/'
                WHEN 'IDENTITY_DOCUMENT' THEN 'identity-documents/'
                WHEN 'CV' THEN 'files/'
            END
            || id::text || '.' ||
            CASE content_type
                WHEN 'image/jpeg' THEN 'jpg'
                WHEN 'image/png' THEN 'png'
                WHEN 'application/pdf' THEN 'pdf'
                WHEN 'application/msword' THEN 'doc'
                WHEN 'application/vnd.openxmlformats-officedocument.wordprocessingml.document' THEN 'docx'
            END)),
    -- The types a kind accepts (internal/media.Kind.AllowedTypes).
    CONSTRAINT media_content_type_check CHECK (
        content_type IS NULL
        OR (kind IN ('PROFILE_PICTURE', 'COMPANY_LOGO', 'COMPANY_IMAGE', 'IDENTITY_DOCUMENT')
            AND content_type IN ('image/jpeg', 'image/png'))
        OR (kind = 'CV' AND content_type IN ('application/pdf', 'application/msword',
            'application/vnd.openxmlformats-officedocument.wordprocessingml.document'))),
    CONSTRAINT media_bound_check CHECK (bound_at IS NULL OR status = 'READY'),
    CONSTRAINT media_declared_size_check CHECK (declared_size BETWEEN 1 AND 10485760),
    CONSTRAINT media_size_check CHECK (size_bytes IS NULL OR size_bytes >= 0),
    CONSTRAINT media_rejection_check CHECK (
        rejection IS NULL OR rejection IN ('NOT_UPLOADED', 'TYPE_UNSUPPORTED', 'TOO_LARGE', 'CORRUPT')),
    -- The state machine's columns (internal/media/domain.go): PENDING has
    -- no outcome, READY has an object and no rejection, REJECTED has a
    -- rejection and no object. Both outcomes are decided.
    CONSTRAINT media_state_check CHECK (
        (status = 'PENDING' AND decided_at IS NULL AND object_key IS NULL AND content_type IS NULL
            AND size_bytes IS NULL AND rejection IS NULL)
        OR (status = 'READY' AND decided_at IS NOT NULL AND object_key IS NOT NULL
            AND content_type IS NOT NULL AND size_bytes IS NOT NULL AND rejection IS NULL)
        OR (status = 'REJECTED' AND decided_at IS NOT NULL AND object_key IS NULL
            AND content_type IS NULL AND size_bytes IS NULL AND rejection IS NOT NULL))
);
-- +koala soft-delete v1 media security_barrier
-- +koala begin generated (make generate); do not edit
ALTER TABLE media
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT media_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX media_deletion_id_idx ON media (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON media TO koala_maint;
CREATE VIEW active_media WITH (security_barrier) AS
    SELECT * FROM media WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_media TO koala_app;
-- +koala end generated

-- The owner's media: the cascade edge's index, the open-upload count.
CREATE INDEX media_owner_created_idx ON media (owner_id, created_at);
-- The sweep: uploads that did not end READY, and READY media nothing
-- bound, by age.
CREATE INDEX media_sweep_created_idx ON media (created_at, id)
    WHERE deleted_at IS NULL AND (status <> 'READY' OR bound_at IS NULL);

CREATE TABLE media_upload_starts (
    -- The started media's id; a start writes one row.
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL,
    created_at timestamptz NOT NULL
);

-- The user's budget window.
CREATE INDEX media_upload_starts_user_created_idx ON media_upload_starts (user_id, created_at);
-- The purge goes by age.
CREATE INDEX media_upload_starts_created_at_idx ON media_upload_starts (created_at);

-- Written and purged by the application (koala_app).
GRANT SELECT, INSERT, DELETE ON media_upload_starts TO koala_app;

-- +goose Down
DROP TABLE media_upload_starts;
DROP VIEW active_media;
DROP TABLE media;
