-- Example of the ADR-0013 soft-delete pattern on one table. Test databases
-- only. The directive line is expanded by `make generate`
-- (internal/tools/softdeletegen, ADR-0020) into the columns, the active_
-- view and the grants; the real deletions table is 0010_deletions.sql.

-- +goose Up
CREATE TABLE example_notes (
    id uuid PRIMARY KEY,
    body text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
-- +koala soft-delete v1 example_notes
-- +koala begin generated (make generate); do not edit
ALTER TABLE example_notes
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT example_notes_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX example_notes_deletion_id_idx ON example_notes (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON example_notes TO koala_maint;
CREATE VIEW active_example_notes AS
    SELECT * FROM example_notes WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_example_notes TO koala_app;
-- +koala end generated

-- +goose Down
DROP VIEW active_example_notes;
DROP TABLE example_notes;
