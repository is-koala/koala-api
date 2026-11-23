-- name: LockOwner :one
-- LockOwner locks the caller's active row FOR UPDATE: every write that
-- adds, removes or points the profile at an address takes it first, so
-- the per-user cap is counted and then spent under it and the primary
-- address link (users.address_id) changes under the same lock the profile
-- patch holds (ADR-0039 Karar 5). Its one writer is SetPrimaryAddress.
SELECT id, status, address_id
FROM active_users
WHERE id = $1
FOR UPDATE;

-- name: SetPrimaryAddress :exec
-- SetPrimaryAddress points the profile at one of the user's addresses or
-- clears the link; the caller holds the user's row (LockOwner) and, for an
-- address, the address's row (OwnAddress).
UPDATE active_users
SET address_id = sqlc.narg(address_id)::uuid
WHERE id = sqlc.arg(id)::uuid;

-- name: CountAddresses :one
-- CountAddresses counts the user's active addresses.
SELECT count(*)
FROM active_addresses
WHERE user_id = $1;

-- name: FindCity :one
-- FindCity finds a city that may be chosen: not retired, or retired but
-- kept (allow_retired: the address names it already).
SELECT id
FROM cities
WHERE id = sqlc.arg(id) AND (retired_at IS NULL OR sqlc.arg(allow_retired)::boolean);

-- name: FindDistrictOf :one
-- FindDistrictOf finds a district of the city that may be chosen
-- (allow_retired as FindCity).
SELECT id
FROM districts
WHERE id = sqlc.arg(id) AND city_id = sqlc.arg(city_id)
  AND (retired_at IS NULL OR sqlc.arg(allow_retired)::boolean);

-- name: InsertAddress :exec
-- InsertAddress adds an address. The caller holds the user's row
-- (LockOwner, then softdelete.LockActiveParent).
INSERT INTO active_addresses (id, user_id, city_id, district_id, neighborhood, thoroughfare, building_number,
    building_name, floor, unit_number, postal_code, additional_directions, provider_place_id, latitude, longitude,
    created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(city_id), sqlc.arg(district_id), sqlc.narg(neighborhood),
    sqlc.narg(thoroughfare), sqlc.narg(building_number), sqlc.narg(building_name), sqlc.narg(floor),
    sqlc.narg(unit_number), sqlc.narg(postal_code), sqlc.narg(additional_directions), sqlc.narg(provider_place_id),
    sqlc.narg(latitude), sqlc.narg(longitude), sqlc.arg(created_at), sqlc.arg(created_at));

-- name: LockAddress :one
-- LockAddress locks one of the user's active addresses; another user's is
-- not found. The references it names come back: a retired one it keeps
-- may stay.
SELECT id, city_id, district_id
FROM active_addresses
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
FOR UPDATE;

-- name: OwnAddress :one
-- OwnAddress locks one of the user's active addresses FOR SHARE, for the
-- profile to point at it: a concurrent deletion waits for the link and
-- then finds it (the restrict edge), or has deleted it first and this
-- finds nothing.
SELECT id
FROM active_addresses
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
FOR SHARE;

-- name: ReplaceAddress :exec
-- ReplaceAddress rewrites one of the user's addresses; the caller holds
-- its row (LockAddress).
UPDATE active_addresses
SET city_id = sqlc.arg(city_id),
    district_id = sqlc.arg(district_id),
    neighborhood = sqlc.narg(neighborhood),
    thoroughfare = sqlc.narg(thoroughfare),
    building_number = sqlc.narg(building_number),
    building_name = sqlc.narg(building_name),
    floor = sqlc.narg(floor),
    unit_number = sqlc.narg(unit_number),
    postal_code = sqlc.narg(postal_code),
    additional_directions = sqlc.narg(additional_directions),
    provider_place_id = sqlc.narg(provider_place_id),
    latitude = sqlc.narg(latitude),
    longitude = sqlc.narg(longitude),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: GetAddress :one
-- GetAddress is one of the user's addresses with the names of its city
-- and district.
SELECT a.id, a.neighborhood, a.thoroughfare, a.building_number, a.building_name, a.floor, a.unit_number,
       a.postal_code, a.additional_directions, a.provider_place_id, a.latitude, a.longitude, a.created_at,
       c.id AS city_id, c.name AS city_name, d.id AS district_id, d.name AS district_name
FROM active_addresses a
JOIN cities c ON c.id = a.city_id
JOIN districts d ON d.id = a.district_id
WHERE a.id = sqlc.arg(id) AND a.user_id = sqlc.arg(user_id);

-- name: ListAddresses :many
-- ListAddresses is the user's addresses, the newest first (the ids are
-- UUIDv7). The list is short (AddressesPerUser), so the page is cut in the
-- service.
SELECT a.id, a.neighborhood, a.thoroughfare, a.building_number, a.building_name, a.floor, a.unit_number,
       a.postal_code, a.additional_directions, a.provider_place_id, a.latitude, a.longitude, a.created_at,
       c.id AS city_id, c.name AS city_name, d.id AS district_id, d.name AS district_name
FROM active_addresses a
JOIN cities c ON c.id = a.city_id
JOIN districts d ON d.id = a.district_id
WHERE a.user_id = sqlc.arg(user_id)
ORDER BY a.id DESC;

-- name: InsertSession :exec
-- InsertSession opens a search session of the user.
INSERT INTO address_search_sessions (id, user_id, provider_token, created_at, expires_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(provider_token), sqlc.arg(created_at), sqlc.arg(expires_at));

-- name: LockSession :one
-- LockSession locks one of the user's search sessions; another user's is
-- not found. Expiry and the end by a resolve are the service's to judge,
-- so that every way a session is gone answers the same (MED-010).
SELECT id, provider_token, expires_at, suggestion_requests, candidates, resolved_at
FROM address_search_sessions
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
FOR UPDATE;

-- name: CountSuggestionRequest :exec
-- CountSuggestionRequest spends one of the session's suggestion requests;
-- the caller holds its row (LockSession).
UPDATE address_search_sessions
SET suggestion_requests = suggestion_requests + 1
WHERE id = $1;

-- name: AppendCandidates :one
-- AppendCandidates adds the place ids a suggestion request offered to the
-- session and returns how many the session holds after it: the new ones
-- are the last of them. Concurrent requests of one session append one
-- after the other under the row's lock, so a candidate's index never
-- moves.
UPDATE address_search_sessions
SET candidates = candidates || sqlc.arg(place_ids)::text[]
WHERE id = sqlc.arg(id)::uuid
RETURNING cardinality(candidates)::integer AS held;

-- name: EndSession :exec
-- EndSession marks the session ended by a resolve; the caller holds its
-- row (LockSession).
UPDATE address_search_sessions
SET resolved_at = sqlc.arg(resolved_at)::timestamptz
WHERE id = sqlc.arg(id)::uuid;

-- name: ReopenSession :exec
-- ReopenSession takes back the end of a resolve whose provider call
-- failed, so the caller may try again; a later resolve's end stays.
UPDATE address_search_sessions
SET resolved_at = NULL
WHERE id = sqlc.arg(id)::uuid AND resolved_at = sqlc.arg(resolved_at)::timestamptz;

-- name: PurgeSessions :execrows
-- PurgeSessions hard-deletes the sessions that expired before the bound
-- (M2).
DELETE FROM address_search_sessions WHERE expires_at < $1;

-- name: LockLookups :exec
-- LockLookups serializes the lookup budget's reservations until the
-- transaction ends: the user's budget, the client address's budget and
-- the service's cap are counted and then spent under one lock, so
-- concurrent reservations cannot pass any of them together (ADR-0039
-- Karar 7). The two-key form keeps the class apart from every other
-- advisory lock.
SELECT pg_advisory_xact_lock(sqlc.arg(lock_class)::integer, 0);

-- name: CountUserLookups :one
-- CountUserLookups counts the user's billed units after since.
SELECT count(*)
FROM address_lookups
WHERE user_id = sqlc.arg(user_id)::uuid AND created_at > sqlc.arg(since)::timestamptz;

-- name: OldestUserLookup :one
-- OldestUserLookup is the user's oldest unit after since: the window
-- frees a place when that one leaves it.
SELECT created_at
FROM address_lookups
WHERE user_id = sqlc.arg(user_id)::uuid AND created_at > sqlc.arg(since)::timestamptz
ORDER BY created_at
LIMIT 1;

-- name: CountClientLookups :one
-- CountClientLookups counts the client address's billed units after since.
SELECT count(*)
FROM address_lookups
WHERE client_key = sqlc.arg(client_key)::text AND created_at > sqlc.arg(since)::timestamptz;

-- name: OldestClientLookup :one
-- OldestClientLookup is the client address's oldest unit after since.
SELECT created_at
FROM address_lookups
WHERE client_key = sqlc.arg(client_key)::text AND created_at > sqlc.arg(since)::timestamptz
ORDER BY created_at
LIMIT 1;

-- name: CountAllLookups :one
-- CountAllLookups counts every billed unit after since: the service's cap.
SELECT count(*)
FROM address_lookups
WHERE created_at > sqlc.arg(since)::timestamptz;

-- name: InsertLookup :exec
-- InsertLookup spends one billed unit. A bounds lookup has neither user
-- nor client address.
INSERT INTO address_lookups (id, user_id, client_key, unit, created_at)
VALUES (sqlc.arg(id)::uuid, sqlc.narg(user_id)::uuid, sqlc.narg(client_key)::text, sqlc.arg(unit)::text,
    sqlc.arg(created_at)::timestamptz);

-- name: PurgeLookups :execrows
-- PurgeLookups hard-deletes the ledger rows written before the bound (M2,
-- a day).
DELETE FROM address_lookups WHERE created_at < $1;
