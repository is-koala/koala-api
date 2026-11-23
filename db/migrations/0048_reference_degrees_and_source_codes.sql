-- The degree of a department and the departments' source codes (ADR-0038).
--
-- departments.degree: a department is a program, and a program is an
-- associate degree program (önlisans, ASSOCIATE) or a bachelor's program
-- (lisans, BACHELOR); the values are EducationDegree's. The rows the table
-- holds before this migration are YÖK's bachelor's programs (ADR-0037), so
-- the column comes with BACHELOR for them and then has no default: the
-- sync writes every row's degree. A department's degree does not change;
-- the sync refuses a row that changes it, as it refuses a moved row.
--
-- department_source_codes: the ÖSYM program codes of a department (YÖK
-- Atlas's "program kodu", nine digits). A department merged from a
-- program's quota and fee variants ("Burslu", "%50 İndirimli") has the
-- code of each, so the table is keyed by the code. M4 like the other
-- reference tables: its rows come from db/referencedata through `koala
-- migrate up`, a code no longer listed is retired (retired_at), never
-- deleted, and a code is never moved to another department. The
-- bachelor's programs of the Java snapshot have no code yet.

-- +goose Up
ALTER TABLE departments
    ADD COLUMN degree text NOT NULL DEFAULT 'BACHELOR',
    ADD CONSTRAINT departments_degree_check CHECK (degree IN ('ASSOCIATE', 'BACHELOR'));
ALTER TABLE departments ALTER COLUMN degree DROP DEFAULT;

CREATE TABLE department_source_codes (
    code text PRIMARY KEY,
    department_id uuid NOT NULL REFERENCES departments (id),
    -- A retired code stays: it named the program once (M4, ADR-0013).
    retired_at timestamptz,
    CONSTRAINT department_source_codes_code_check CHECK (code ~ '^[0-9]{9}$')
);

CREATE INDEX department_source_codes_department_idx ON department_source_codes (department_id);

-- Reference data: read-only for the application, as the other reference
-- tables; its rows come from db/referencedata through koala migrate up as
-- koala_owner (ADR-0037).
GRANT SELECT ON department_source_codes TO koala_app;

-- +goose Down
DROP TABLE department_source_codes;
ALTER TABLE departments DROP COLUMN degree;
