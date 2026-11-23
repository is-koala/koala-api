-- The caller's addresses, the profile's primary address, the address
-- search sessions and the address lookup budget's ledger (ADR-0029 Karar 8,
-- ADR-0036 Karar 5, ADR-0039).
--
-- addresses: one address of one user, in our own components: the city and
-- the district (M4 reference rows; the district is held to its city by a
-- composite foreign key, as an education holds its faculty), the free
-- lines, the postal code, the provider's place id and the coordinates.
-- The provider's formatted address and raw answer are never stored
-- (MED-008): there is no column for them. The coordinates are kept at full
-- precision; the projection that renders them decides what a caller sees
-- (MED-009). components_source and coordinate_source say whether the
-- provider gave them (ADR-0039 Karar 10). Soft-deleted with the user (cascade) and personal data, hence
-- the security barrier.
--
-- users.address_id: the profile's primary address, a restrict edge to
-- addresses like the profile's media (ADR-0036 Karar 10). The address
-- module is its one writer; a deletion of the primary address clears it
-- in its own transaction first (ADR-0039 Karar 5). That the address is the
-- user's own is held by the code under the user's row lock, not by a
-- composite key: the soft-delete catalog reads one column per edge.
--
-- address_search_sessions (M2): a search session of the provider, bound
-- to its user and alive 15 minutes (MED-010). provider_token is a random
-- UUIDv4, the provider's session token, tied to nothing of the user. The
-- candidates are the place ids the session's suggestions offered, in
-- order: place ids are the one piece of the provider's content that may be
-- stored without limit (Google Maps Platform policies), and nothing else
-- of a suggestion is written. No foreign key: a session outlives nothing
-- and its caller no longer authenticates once the user is gone.
--
-- address_area_bounds (M5): the service area cities' bounds, looked up
-- from the provider and shared by every instance, so their cost does not
-- grow with the instances or a restart (ADR-0039 Karar 3). It is the
-- provider's content: a coordinate rectangle, which its terms let us keep
-- at most 30 consecutive days; a row older than that is not used and the
-- address purge deletes it. retry_at holds the backoff of a failed lookup
-- and the claim of a running one, so one instance looks a city up at a
-- time.
--
-- address_lookups (M2): the lookup budget's ledger, one row per unit (a
-- session, an autocomplete request, a place details call, a reverse
-- geocode, a service area bounds lookup), with the user, the client
-- address it came from and
-- the time. The user's daily budget, the client address's hourly budget
-- and the service's daily cap count it (ADR-0039 Karar 7). A bounds lookup
-- is the service's own and has neither user nor client address. Hard-
-- deleted a day after; no foreign key, like media_upload_starts.

-- +goose Up
CREATE TABLE addresses (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    city_id uuid NOT NULL REFERENCES cities (id),
    district_id uuid NOT NULL,
    neighborhood text,
    thoroughfare text,
    building_number text,
    building_name text,
    site_name text,
    block text,
    floor text,
    unit_number text,
    postal_code text,
    additional_directions text,
    provider_place_id text,
    latitude double precision,
    longitude double precision,
    -- Where the components and the coordinate came from (ADR-0039 Karar
    -- 10): a legal answer that the provider's content may not be kept for
    -- good would find the rows to clean up by these.
    components_source text NOT NULL,
    coordinate_source text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    -- A district is chosen under its city.
    CONSTRAINT addresses_district_fkey FOREIGN KEY (district_id, city_id) REFERENCES districts (id, city_id),
    CONSTRAINT addresses_neighborhood_check CHECK (length(neighborhood) BETWEEN 1 AND 120),
    CONSTRAINT addresses_thoroughfare_check CHECK (length(thoroughfare) BETWEEN 1 AND 160),
    CONSTRAINT addresses_building_number_check CHECK (length(building_number) BETWEEN 1 AND 30),
    CONSTRAINT addresses_building_name_check CHECK (length(building_name) BETWEEN 1 AND 120),
    CONSTRAINT addresses_site_name_check CHECK (length(site_name) BETWEEN 1 AND 120),
    CONSTRAINT addresses_block_check CHECK (length(block) BETWEEN 1 AND 30),
    CONSTRAINT addresses_floor_check CHECK (length(floor) BETWEEN 1 AND 20),
    CONSTRAINT addresses_unit_number_check CHECK (length(unit_number) BETWEEN 1 AND 30),
    CONSTRAINT addresses_postal_code_check CHECK (postal_code ~ '^[0-9]{5}$'),
    CONSTRAINT addresses_additional_directions_check CHECK (length(additional_directions) BETWEEN 1 AND 500),
    CONSTRAINT addresses_provider_place_id_check CHECK (length(provider_place_id) BETWEEN 1 AND 255),
    -- Both coordinates or neither, each in its range.
    CONSTRAINT addresses_coordinates_check CHECK (
        (latitude IS NULL) = (longitude IS NULL)
        AND (latitude IS NULL OR latitude BETWEEN -90 AND 90)
        AND (longitude IS NULL OR longitude BETWEEN -180 AND 180)),
    CONSTRAINT addresses_components_source_check CHECK (components_source IN ('MANUAL', 'AUTOCOMPLETE', 'REVERSE_GEOCODE')),
    -- A provider's place id comes with the provider's components and only
    -- with them (ADR-0039 Karar 10).
    CONSTRAINT addresses_place_id_source_check CHECK ((components_source = 'MANUAL') = (provider_place_id IS NULL)),
    -- A reverse geocode's coordinate is the caller's pin, never the
    -- provider's.
    CONSTRAINT addresses_reverse_pin_check CHECK (
        NOT (components_source = 'REVERSE_GEOCODE' AND coordinate_source = 'PROVIDER')),
    -- A coordinate has a source, and only a coordinate has one.
    CONSTRAINT addresses_coordinate_source_check CHECK (
        (coordinate_source IS NULL) = (latitude IS NULL)
        AND (coordinate_source IS NULL OR coordinate_source IN ('USER_PIN', 'PROVIDER'))),
    CONSTRAINT addresses_times_check CHECK (updated_at >= created_at)
);
-- +koala soft-delete v1 addresses security_barrier
-- +koala begin generated (make generate); do not edit
ALTER TABLE addresses
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deletion_id uuid REFERENCES deletions (id),
    ADD CONSTRAINT addresses_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));
CREATE INDEX addresses_deletion_id_idx ON addresses (deletion_id) WHERE deletion_id IS NOT NULL;
GRANT SELECT, INSERT, UPDATE, DELETE ON addresses TO koala_maint;
CREATE VIEW active_addresses WITH (security_barrier) AS
    SELECT * FROM addresses WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_addresses TO koala_app;
-- +koala end generated

-- The cascade edge addresses.user_id → users, and the owner's list.
CREATE INDEX addresses_user_idx ON addresses (user_id);

DROP VIEW active_users;
ALTER TABLE users ADD COLUMN address_id uuid REFERENCES addresses (id);
-- +koala soft-delete-view v1 users security_barrier
-- +koala begin generated (make generate); do not edit
CREATE VIEW active_users WITH (security_barrier) AS
    SELECT * FROM users WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_users TO koala_app;
-- +koala end generated

-- The restrict edge users.address_id → addresses: a deletion of an address
-- looks its consumers up by this column.
CREATE INDEX users_address_idx ON users (address_id);

CREATE TABLE address_search_sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL,
    provider_token uuid NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    -- How many suggestion requests the session made (ADR-0039 Karar 7).
    suggestion_requests integer NOT NULL DEFAULT 0,
    -- The place ids the suggestions offered, in order; a candidate is its
    -- index here.
    candidates text[] NOT NULL DEFAULT '{}',
    -- Set when the session ended: by a resolve (the provider ends its
    -- session with the place details call) or by its request bound.
    ended_at timestamptz,
    CONSTRAINT address_search_sessions_times_check CHECK (expires_at > created_at),
    CONSTRAINT address_search_sessions_requests_check CHECK (suggestion_requests >= 0),
    CONSTRAINT address_search_sessions_candidates_check CHECK (cardinality(candidates) <= 400),
    CONSTRAINT address_search_sessions_token_key UNIQUE (provider_token)
);

-- The caller's session is found by (id, user_id); the purge goes by
-- expiry.
CREATE INDEX address_search_sessions_expires_at_idx ON address_search_sessions (expires_at);

CREATE TABLE address_area_bounds (
    city_id uuid PRIMARY KEY REFERENCES cities (id),
    south double precision,
    west double precision,
    north double precision,
    east double precision,
    fetched_at timestamptz,
    failures integer NOT NULL DEFAULT 0,
    retry_at timestamptz,
    CONSTRAINT address_area_bounds_rect_check CHECK (
        (fetched_at IS NULL) = (south IS NULL) AND (south IS NULL) = (west IS NULL)
        AND (west IS NULL) = (north IS NULL) AND (north IS NULL) = (east IS NULL)
        AND (south IS NULL OR (south <= north AND west <= east))),
    CONSTRAINT address_area_bounds_failures_check CHECK (failures >= 0)
);

CREATE TABLE address_lookups (
    id uuid PRIMARY KEY,
    -- NULL for the service's own bounds lookup.
    user_id uuid,
    -- The client address (an IPv6 client by its /64); NULL with user_id.
    client_key text,
    unit text NOT NULL,
    created_at timestamptz NOT NULL,
    CONSTRAINT address_lookups_unit_check CHECK (unit IN ('SESSION', 'AUTOCOMPLETE', 'DETAILS', 'REVERSE', 'BOUNDS')),
    CONSTRAINT address_lookups_caller_check CHECK (
        (user_id IS NULL) = (client_key IS NULL) AND (user_id IS NULL) = (unit = 'BOUNDS')),
    CONSTRAINT address_lookups_client_key_check CHECK (length(client_key) BETWEEN 1 AND 64)
);

-- The user's and the client address's budget windows; the service's cap
-- and the purge go by age.
CREATE INDEX address_lookups_user_created_idx ON address_lookups (user_id, created_at);
CREATE INDEX address_lookups_client_created_idx ON address_lookups (client_key, created_at);
CREATE INDEX address_lookups_created_at_idx ON address_lookups (created_at);

-- Written, read and purged by the application (koala_app).
GRANT SELECT, INSERT, UPDATE, DELETE ON address_search_sessions TO koala_app;
GRANT SELECT, INSERT, DELETE ON address_lookups TO koala_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON address_area_bounds TO koala_app;

-- +goose Down
DROP TABLE address_lookups;
DROP TABLE address_area_bounds;
DROP TABLE address_search_sessions;
DROP VIEW active_users;
ALTER TABLE users DROP COLUMN address_id;
CREATE VIEW active_users WITH (security_barrier) AS
    SELECT * FROM users WHERE deleted_at IS NULL;
GRANT SELECT, INSERT, UPDATE ON active_users TO koala_app;
DROP VIEW active_addresses;
DROP TABLE addresses;
