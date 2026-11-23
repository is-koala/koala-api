-- Place reference data and the Turkish collation of reference names
-- (ADR-0029 Karar 8, ADR-0037).
--
-- countries, cities, districts: M4 like the academic tables of 0046,
-- read-only for the application. A row is retired (retired_at), never
-- deleted. A city belongs to a country, a district to a city; (id, parent)
-- is unique so an address can hold its district to its city with a
-- composite foreign key, as an education does with its faculty
-- (ADR-0036 Karar 6). Countries carry their ISO 3166-1 alpha-2 code.
--
-- The rows of all six reference tables are not written here: they come
-- from the versioned CSV files of db/referencedata, which `koala migrate
-- up` brings the tables in line with after the migrations (insert the
-- new, update the changed, retire the dropped; ADR-0037).
--
-- reference_data_version: the version of db/referencedata the tables were
-- last brought in line with (db/referencedata/VERSION). A sync with an
-- older version writes nothing, so a rolled-back release does not take the
-- data back. revision grows with every sync that writes a row, also one
-- that repairs a drift under the same version; the lists derive their
-- ETag from both. One row.
--
-- turkish: an ICU collation for the reference names. The lists are
-- ordered by name and the names are Turkish: under the byte order Ç, Ğ,
-- İ, Ö, Ş and Ü sort after Z, and under the root collation Ş sorts with
-- S and İ with I. The collation belongs to the column, so every ORDER BY
-- name uses it. A server built without ICU fails here, loudly.

-- +goose Up
CREATE COLLATION turkish (provider = icu, locale = 'tr-TR');

ALTER TABLE universities ALTER COLUMN name TYPE text COLLATE turkish;
ALTER TABLE faculties ALTER COLUMN name TYPE text COLLATE turkish;
ALTER TABLE departments ALTER COLUMN name TYPE text COLLATE turkish;

CREATE TABLE countries (
    id uuid PRIMARY KEY,
    code text NOT NULL,
    name text COLLATE turkish NOT NULL,
    -- A retired row stays for the records that name it and cannot be
    -- chosen anew (M4, ADR-0013).
    retired_at timestamptz,
    CONSTRAINT countries_code_check CHECK (code ~ '^[A-Z]{2}$'),
    CONSTRAINT countries_code_key UNIQUE (code),
    CONSTRAINT countries_name_check CHECK (length(name) BETWEEN 1 AND 200)
);

CREATE TABLE cities (
    id uuid PRIMARY KEY,
    country_id uuid NOT NULL REFERENCES countries (id),
    name text COLLATE turkish NOT NULL,
    retired_at timestamptz,
    CONSTRAINT cities_name_check CHECK (length(name) BETWEEN 1 AND 200),
    CONSTRAINT cities_country_key UNIQUE (id, country_id)
);

CREATE INDEX cities_country_idx ON cities (country_id);

CREATE TABLE districts (
    id uuid PRIMARY KEY,
    city_id uuid NOT NULL REFERENCES cities (id),
    name text COLLATE turkish NOT NULL,
    retired_at timestamptz,
    CONSTRAINT districts_name_check CHECK (length(name) BETWEEN 1 AND 200),
    CONSTRAINT districts_city_key UNIQUE (id, city_id)
);

CREATE INDEX districts_city_idx ON districts (city_id);

CREATE TABLE reference_data_version (
    singleton boolean PRIMARY KEY DEFAULT true,
    version integer NOT NULL,
    revision integer NOT NULL,
    synced_at timestamptz NOT NULL,
    CONSTRAINT reference_data_version_singleton_check CHECK (singleton),
    CONSTRAINT reference_data_version_version_check CHECK (version >= 1),
    CONSTRAINT reference_data_version_revision_check CHECK (revision >= 1)
);

-- Reference data: read-only for the application; its rows come from
-- db/referencedata through `koala migrate up` as koala_owner (ADR-0037).
GRANT SELECT ON countries TO koala_app;
GRANT SELECT ON cities TO koala_app;
GRANT SELECT ON districts TO koala_app;
-- The lists' ETag.
GRANT SELECT ON reference_data_version TO koala_app;

-- +goose Down
DROP TABLE reference_data_version;
DROP TABLE districts;
DROP TABLE cities;
DROP TABLE countries;
ALTER TABLE departments ALTER COLUMN name TYPE text COLLATE "default";
ALTER TABLE faculties ALTER COLUMN name TYPE text COLLATE "default";
ALTER TABLE universities ALTER COLUMN name TYPE text COLLATE "default";
DROP COLLATION turkish;
