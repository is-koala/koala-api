-- +goose NO TRANSACTION
-- Example of a migration outside a transaction: CREATE INDEX CONCURRENTLY
-- does not block writes but refuses to run in a transaction block. One
-- statement per such file: goose cannot roll a half-applied file back. If
-- the build fails it leaves an INVALID index; drop it before retrying (no
-- IF NOT EXISTS, which would silently keep the invalid index).

-- +goose Up
CREATE INDEX CONCURRENTLY example_notes_active_created_at_idx
    ON example_notes (created_at)
    WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS example_notes_active_created_at_idx;
