-- The record of soft deletions (ADR-0013, ADR-0020). One row per call of
-- softdelete.Delete: who deleted what and why. Every row the call marks,
-- the root and its cascade, carries the row's id in deletion_id, so a
-- restore reopens exactly those rows and not rows deleted on their own.
--
-- The table is evidence (exemption class M1, db/softdelete.yaml): rows are
-- never deleted or edited, except that a restore stamps restored_at and
-- the restoring actor once. koala_app only inserts: softdelete.Delete runs
-- in the application's transaction and makes the id itself, so it needs no
-- SELECT and no RETURNING. koala_maint reads and stamps the restore columns
-- only.

-- +goose Up
CREATE TABLE deletions (
    id uuid PRIMARY KEY,
    root_table text NOT NULL,
    root_id uuid NOT NULL,
    actor_kind text NOT NULL,
    actor_id uuid,
    reason text NOT NULL,
    created_at timestamptz NOT NULL,
    restored_at timestamptz,
    restored_by_kind text,
    restored_by uuid,
    CONSTRAINT deletions_actor_kind_check CHECK (actor_kind IN ('user', 'staff', 'system')),
    -- A person deletes with an id; the system (retention, cascades of jobs)
    -- has none.
    CONSTRAINT deletions_actor_id_check CHECK ((actor_kind = 'system') = (actor_id IS NULL)),
    CONSTRAINT deletions_reason_check CHECK (reason IN ('user_request', 'staff_action', 'retention', 'account_deletion')),
    CONSTRAINT deletions_restored_after_created CHECK (restored_at >= created_at),
    -- A restore is stamped whole: time and actor kind together, and the
    -- actor's id exactly when a person restored.
    CONSTRAINT deletions_restored_by_kind_check CHECK (restored_by_kind IN ('user', 'staff', 'system')),
    CONSTRAINT deletions_restored_together CHECK ((restored_at IS NULL) = (restored_by_kind IS NULL)),
    CONSTRAINT deletions_restored_by_check CHECK (
        CASE WHEN restored_by_kind IS NULL THEN restored_by IS NULL
             ELSE (restored_by_kind = 'system') = (restored_by IS NULL)
        END)
);

CREATE INDEX deletions_root_idx ON deletions (root_table, root_id);

GRANT INSERT ON deletions TO koala_app;
GRANT SELECT ON deletions TO koala_maint;
GRANT UPDATE (restored_at, restored_by_kind, restored_by) ON deletions TO koala_maint;

-- +goose Down
DROP TABLE deletions;
