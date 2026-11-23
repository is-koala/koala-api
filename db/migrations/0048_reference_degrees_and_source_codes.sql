-- The degree of a department and the departments' source codes (ADR-0038).
--
-- departments.degree: a department is a program, and a program is an
-- associate degree program (önlisans, ASSOCIATE) or a bachelor's program
-- (lisans, BACHELOR); the values are EducationDegree's. The rows the table
-- holds before this migration are YÖK's bachelor's programs (data version
-- 1, ADR-0037), so the column comes with BACHELOR for them and then has no
-- default: the sync writes every row's degree. A department's degree does
-- not change; the sync refuses a row that changes it, as it refuses a
-- moved row.
--
-- department_source_codes: the ÖSYM program codes of a department (YÖK
-- Atlas's "program kodu", nine digits). A department merged from a
-- program's quota and fee variants ("Burslu", "%50 İndirimli") has the
-- code of each. A row is a (code, department) pair and never changes; a
-- code ÖSYM gives to another program, decided in the id ledger's
-- decisions (db/referencedata/tools), is another pair: the old one is
-- retired, never deleted, and at most one pair per code is in use. M4
-- like the other reference tables: its rows come from db/referencedata
-- through `koala migrate up`. The bachelor's programs of the Java snapshot
-- have no code yet.
--
-- Down is a development tool (`koala migrate` refuses it in production).
-- Without the degree column the associate degree programs cannot be told
-- from the bachelor's ones: left in place, an up would make them BACHELOR
-- and the next sync would refuse every one of them as a changed degree.
-- So down deletes them first, and stops with a message if an education
-- record names one (delete the record first). The data version is kept:
-- the next up's sync puts the rows back under their ids, as the repair of
-- a drift, and an older binary leaves a newer version as it is
-- (ADR-0037 Karar 3b).

-- +goose Up
ALTER TABLE departments
    ADD COLUMN degree text NOT NULL DEFAULT 'BACHELOR',
    ADD CONSTRAINT departments_degree_check CHECK (degree IN ('ASSOCIATE', 'BACHELOR'));
ALTER TABLE departments ALTER COLUMN degree DROP DEFAULT;

CREATE TABLE department_source_codes (
    code text NOT NULL,
    department_id uuid NOT NULL REFERENCES departments (id),
    -- A retired pair stays: the code named that program once (M4,
    -- ADR-0013).
    retired_at timestamptz,
    PRIMARY KEY (code, department_id),
    CONSTRAINT department_source_codes_code_check CHECK (code ~ '^[0-9]{9}$')
);

-- One program per code at a time.
CREATE UNIQUE INDEX department_source_codes_in_use_key ON department_source_codes (code) WHERE retired_at IS NULL;
CREATE INDEX department_source_codes_department_idx ON department_source_codes (department_id);

-- Reference data: read-only for the application, as the other reference
-- tables; its rows come from db/referencedata through koala migrate up as
-- koala_owner (ADR-0037).
GRANT SELECT ON department_source_codes TO koala_app;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT FROM educations e JOIN departments d ON d.id = e.department_id WHERE d.degree = 'ASSOCIATE'
    ) THEN
        RAISE EXCEPTION 'migration 0048 down: education records name associate degree programs, which down deletes; delete those records first (ADR-0038)';
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE department_source_codes;
DELETE FROM departments WHERE degree = 'ASSOCIATE';
ALTER TABLE departments DROP COLUMN degree;
