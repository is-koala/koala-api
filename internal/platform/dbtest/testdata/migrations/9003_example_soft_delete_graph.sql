-- Example tables for the soft-delete platform's tests (ADR-0020). Test
-- databases only; their catalog is ../softdelete.yaml. One edge of each
-- policy, two cascade levels and a partial unique key:
--
--   example_owners      (pii: security_barrier view)
--     <- example_projects.owner_id   cascade
--          <- example_tasks.project_id      cascade
--          <- example_invoices.project_id   restrict
--     <- example_comments.author_id  detach

-- +goose Up
CREATE TABLE example_owners (
    id uuid PRIMARY KEY,
    name text NOT NULL
);
-- +koala soft-delete v1 example_owners security_barrier
-- +koala begin generated (make generate); do not edit
ALTER TABLE example_owners
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT example_owners_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX example_owners_deletion_id_idx ON example_owners (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON example_owners TO koala_maint;
CREATE VIEW active_example_owners WITH (security_barrier) AS
    SELECT * FROM example_owners WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_example_owners TO koala_app;
-- +koala end generated

CREATE TABLE example_projects (
    id uuid PRIMARY KEY,
    owner_id uuid NOT NULL REFERENCES example_owners (id),
    name text NOT NULL
);
-- +koala soft-delete v1 example_projects
-- +koala begin generated (make generate); do not edit
ALTER TABLE example_projects
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT example_projects_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX example_projects_deletion_id_idx ON example_projects (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON example_projects TO koala_maint;
CREATE VIEW active_example_projects AS
    SELECT * FROM example_projects WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_example_projects TO koala_app;
-- +koala end generated

-- A task name is unique among the project's active tasks; a restore that
-- would duplicate it is a RESTORE_CONFLICT.
CREATE TABLE example_tasks (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES example_projects (id),
    name text NOT NULL
);
-- +koala soft-delete v1 example_tasks
-- +koala begin generated (make generate); do not edit
ALTER TABLE example_tasks
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT example_tasks_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX example_tasks_deletion_id_idx ON example_tasks (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON example_tasks TO koala_maint;
CREATE VIEW active_example_tasks AS
    SELECT * FROM example_tasks WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_example_tasks TO koala_app;
-- +koala end generated
CREATE UNIQUE INDEX example_tasks_project_name_key
    ON example_tasks (project_id, name)
    WHERE deleted_at IS NULL;

CREATE TABLE example_invoices (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES example_projects (id),
    amount bigint NOT NULL
);
-- +koala soft-delete v1 example_invoices
-- +koala begin generated (make generate); do not edit
ALTER TABLE example_invoices
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT example_invoices_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX example_invoices_deletion_id_idx ON example_invoices (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON example_invoices TO koala_maint;
CREATE VIEW active_example_invoices AS
    SELECT * FROM example_invoices WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_example_invoices TO koala_app;
-- +koala end generated

CREATE TABLE example_comments (
    id uuid PRIMARY KEY,
    author_id uuid NOT NULL REFERENCES example_owners (id),
    body text NOT NULL
);
-- +koala soft-delete v1 example_comments
-- +koala begin generated (make generate); do not edit
ALTER TABLE example_comments
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT example_comments_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX example_comments_deletion_id_idx ON example_comments (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON example_comments TO koala_maint;
CREATE VIEW active_example_comments AS
    SELECT * FROM example_comments WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_example_comments TO koala_app;
-- +koala end generated

-- +goose Down
DROP VIEW active_example_comments;
DROP TABLE example_comments;
DROP VIEW active_example_invoices;
DROP TABLE example_invoices;
DROP VIEW active_example_tasks;
DROP TABLE example_tasks;
DROP VIEW active_example_projects;
DROP TABLE example_projects;
DROP VIEW active_example_owners;
DROP TABLE example_owners;
