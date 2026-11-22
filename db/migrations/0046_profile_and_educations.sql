-- The caller's profile, education records and the profile sync intent
-- (ADR-0029 Karar 11, ADR-0036).
--
-- users gains the profile fields of GET/PATCH /v1/me/profile. The address,
-- the phone and the e-mail are not among them: the e-mail and the phone
-- are written only by a confirmed verification (USR-014, 0043), and the
-- profile picture, the CV and the primary address are links to the media
-- and address tables, which their own PRs bring together with the columns
-- and foreign keys (ADR-0036 Karar 5).
--
-- universities, faculties, departments: the academic reference data (M4,
-- ADR-0029 Karar 8), read-only for the application. A row is retired
-- (retired_at), never deleted, and a retired one cannot be chosen anew.
-- Their rows come with the reference data PR, which also serves
-- /v1/reference (ADR-0036 Karar 7); this migration brings the tables the
-- education records point at.
-- (id, parent) is unique on faculties and departments so an education's
-- composite foreign keys hold a faculty to its university and a department
-- to its faculty in the database too.
--
-- educations: the caller's education records, soft-deleted with the user
-- (cascade). An education is personal data (KVKK), hence the security
-- barrier.
--
-- user_profile_sync_intents (M2): the outbox of the payment provider's
-- profile sync (USR-022; ADR-0029 Sonuçlar, ADR-0033 Karar 8). A change of the
-- profile writes, in its own transaction, the user's one open intent, or
-- refreshes it: the time and the origin of the latest change (request,
-- session, device). No payload is stored: the dispatcher of Faz 6 reads
-- the profile as it is when it publishes, so the intent never carries
-- personal data of its own and never goes stale. published_at is set by
-- that dispatcher.

-- +goose Up
DROP VIEW active_users;
ALTER TABLE users
    ADD COLUMN birth_date date,
    ADD COLUMN gender text,
    ADD COLUMN job_title text,
    ADD COLUMN about text,
    ADD COLUMN qualifications text,
    ADD COLUMN interests text,
    ADD CONSTRAINT users_gender_check CHECK (gender IN ('MALE', 'FEMALE', 'NOT_SPECIFIED')),
    ADD CONSTRAINT users_birth_date_check CHECK (birth_date >= DATE '1900-01-01'),
    ADD CONSTRAINT users_job_title_check CHECK (length(job_title) BETWEEN 1 AND 120),
    ADD CONSTRAINT users_about_check CHECK (length(about) BETWEEN 1 AND 4000),
    ADD CONSTRAINT users_qualifications_check CHECK (length(qualifications) BETWEEN 1 AND 4000),
    ADD CONSTRAINT users_interests_check CHECK (length(interests) BETWEEN 1 AND 4000);
-- +koala soft-delete-view v1 users security_barrier
-- +koala begin generated (make generate); do not edit
CREATE VIEW active_users WITH (security_barrier) AS
    SELECT * FROM users WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_users TO koala_app;
-- +koala end generated

CREATE TABLE universities (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    -- A retired row stays for the records that name it and cannot be
    -- chosen anew (M4, ADR-0013).
    retired_at timestamptz,
    CONSTRAINT universities_name_check CHECK (length(name) BETWEEN 1 AND 200)
);

CREATE TABLE faculties (
    id uuid PRIMARY KEY,
    university_id uuid NOT NULL REFERENCES universities (id),
    name text NOT NULL,
    retired_at timestamptz,
    CONSTRAINT faculties_name_check CHECK (length(name) BETWEEN 1 AND 200),
    CONSTRAINT faculties_university_key UNIQUE (id, university_id)
);

CREATE INDEX faculties_university_idx ON faculties (university_id);

CREATE TABLE departments (
    id uuid PRIMARY KEY,
    faculty_id uuid NOT NULL REFERENCES faculties (id),
    name text NOT NULL,
    retired_at timestamptz,
    CONSTRAINT departments_name_check CHECK (length(name) BETWEEN 1 AND 200),
    CONSTRAINT departments_faculty_key UNIQUE (id, faculty_id)
);

CREATE INDEX departments_faculty_idx ON departments (faculty_id);

CREATE TABLE educations (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    university_id uuid NOT NULL REFERENCES universities (id),
    faculty_id uuid,
    department_id uuid,
    degree text NOT NULL,
    start_year integer NOT NULL,
    end_year integer,
    created_at timestamptz NOT NULL,
    CONSTRAINT educations_degree_check CHECK (degree IN ('HIGH_SCHOOL', 'ASSOCIATE', 'BACHELOR', 'MASTER', 'DOCTORATE')),
    CONSTRAINT educations_years_check CHECK (
        start_year BETWEEN 1950 AND 2100 AND (end_year IS NULL OR end_year BETWEEN start_year AND 2100)),
    -- A department is chosen under a faculty, a faculty under the
    -- university: the composite keys hold both.
    CONSTRAINT educations_department_needs_faculty CHECK (department_id IS NULL OR faculty_id IS NOT NULL),
    CONSTRAINT educations_faculty_fkey FOREIGN KEY (faculty_id, university_id)
        REFERENCES faculties (id, university_id),
    CONSTRAINT educations_department_fkey FOREIGN KEY (department_id, faculty_id)
        REFERENCES departments (id, faculty_id)
);
-- +koala soft-delete v1 educations security_barrier
-- +koala begin generated (make generate); do not edit
ALTER TABLE educations
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT educations_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX educations_deletion_id_idx ON educations (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON educations TO koala_maint;
CREATE VIEW active_educations WITH (security_barrier) AS
    SELECT * FROM educations WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_educations TO koala_app;
-- +koala end generated

-- The cascade edge educations.user_id → users, and the owner's list.
CREATE INDEX educations_user_idx ON educations (user_id);

CREATE TABLE user_profile_sync_intents (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    -- The origin of the latest change the intent stands for.
    request_id text,
    session_id uuid,
    device_key text,
    published_at timestamptz,
    CONSTRAINT user_profile_sync_intents_times_check CHECK (updated_at >= created_at),
    CONSTRAINT user_profile_sync_intents_request_id_check CHECK (length(request_id) BETWEEN 1 AND 128),
    CONSTRAINT user_profile_sync_intents_device_key_check CHECK (device_key ~ '^[A-Za-z0-9._:-]{8,128}$')
);

-- One open intent per user: a change while one is open refreshes it.
CREATE UNIQUE INDEX user_profile_sync_intents_open_key ON user_profile_sync_intents (user_id)
    WHERE published_at IS NULL;
CREATE INDEX user_profile_sync_intents_user_idx ON user_profile_sync_intents (user_id);

-- Reference data: read-only for the application; its rows come by
-- migration (ADR-0029 Karar 8).
GRANT SELECT ON universities TO koala_app;
GRANT SELECT ON faculties TO koala_app;
GRANT SELECT ON departments TO koala_app;
-- The outbox: written by the profile changes, read and marked published
-- by the dispatcher (Faz 6), never deleted by the application yet.
GRANT SELECT, INSERT, UPDATE ON user_profile_sync_intents TO koala_app;

-- +goose Down
DROP TABLE user_profile_sync_intents;
DROP VIEW active_educations;
DROP TABLE educations;
DROP TABLE departments;
DROP TABLE faculties;
DROP TABLE universities;
DROP VIEW active_users;
ALTER TABLE users
    DROP COLUMN interests,
    DROP COLUMN qualifications,
    DROP COLUMN about,
    DROP COLUMN job_title,
    DROP COLUMN gender,
    DROP COLUMN birth_date;
CREATE VIEW active_users WITH (security_barrier) AS
    SELECT * FROM users WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_users TO koala_app;
