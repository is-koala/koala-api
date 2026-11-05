-- Example of the ADR-0013 soft-delete pattern. Test databases only: Faz 1
-- writes the real deletions table and the goose helpers for views and
-- grants. Kept here so the role model is proven on a concrete table.

-- +goose Up
CREATE TABLE example_deletions (
    id uuid PRIMARY KEY,
    reason text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE example_notes (
    id uuid PRIMARY KEY,
    body text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    deletion_id uuid REFERENCES example_deletions (id),
    CONSTRAINT example_notes_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL))
);

-- The application reads and writes only this view. No CHECK OPTION: the
-- soft delete is an UPDATE through the view that moves the row out of it.
CREATE VIEW active_example_notes AS
    SELECT * FROM example_notes WHERE deleted_at IS NULL;

-- deletions is insert-only for the application (a record, never edited).
GRANT SELECT, INSERT ON example_deletions TO koala_app;
GRANT SELECT, INSERT, UPDATE ON active_example_notes TO koala_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON example_deletions, example_notes TO koala_maint;

-- +goose Down
DROP VIEW active_example_notes;
DROP TABLE example_notes;
DROP TABLE example_deletions;
