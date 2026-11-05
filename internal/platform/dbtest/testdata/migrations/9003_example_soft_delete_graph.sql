-- Example tables for the soft-delete platform's tests (ADR-0020). Test
-- databases only; their catalog is ../softdelete.yaml. Every policy, two
-- cascade levels, a partial unique key, a self-reference and an exempt
-- child:
--
--   example_owners      (pii: security_barrier view)
--     <- example_projects.owner_id   cascade
--          <- example_tasks.project_id      cascade
--               <- example_invoices.task_id     cascade
--          <- example_invoices.project_id   restrict
--          <- example_receipts.project_id   restrict (M1, exempt)
--     <- example_comments.author_id  detach
--   example_folders.parent_id -> example_folders  cascade (a tree)
--
-- An invoice is reached by a restrict edge from its project and by a
-- cascade edge from its task: deleting the owner takes the invoice along
-- (no 409), whatever the order of the edges.

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
CREATE INDEX example_projects_owner_id_idx ON example_projects (owner_id);

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
CREATE INDEX example_tasks_project_id_idx ON example_tasks (project_id);
CREATE UNIQUE INDEX example_tasks_project_name_key
    ON example_tasks (project_id, name)
    WHERE deleted_at IS NULL;

CREATE TABLE example_invoices (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES example_projects (id),
    task_id uuid REFERENCES example_tasks (id),
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
CREATE INDEX example_invoices_project_id_idx ON example_invoices (project_id);
CREATE INDEX example_invoices_task_id_idx ON example_invoices (task_id);

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

CREATE TABLE example_folders (
    id uuid PRIMARY KEY,
    parent_id uuid REFERENCES example_folders (id),
    name text NOT NULL
);
-- +koala soft-delete v1 example_folders
-- +koala begin generated (make generate); do not edit
ALTER TABLE example_folders
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT example_folders_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX example_folders_deletion_id_idx ON example_folders (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON example_folders TO koala_maint;
CREATE VIEW active_example_folders AS
    SELECT * FROM example_folders WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_example_folders TO koala_app;
-- +koala end generated
CREATE INDEX example_folders_parent_id_idx ON example_folders (parent_id);

-- Exempt (M1): never deleted. Its rows hold their project forever
-- (restrict); koala_app inserts and reads it directly.
CREATE TABLE example_receipts (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES example_projects (id),
    amount bigint NOT NULL
);
CREATE INDEX example_receipts_project_id_idx ON example_receipts (project_id);
GRANT SELECT, INSERT ON example_receipts TO koala_app;
GRANT SELECT ON example_receipts TO koala_maint;

-- +goose Down
DROP TABLE example_receipts;
DROP VIEW active_example_folders;
DROP TABLE example_folders;
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
