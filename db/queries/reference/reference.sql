-- The reference lists (ADR-0037). Reference tables are M4: read-only for
-- the application, a row is retired (retired_at), never deleted. A list
-- shows the rows in use, ordered by name under the tables' Turkish
-- collation (0047) and then by id, so the order is total.
--
-- A child list reads its parent with a LEFT JOIN: no row means the parent
-- is missing or retired (404), one row with a NULL child means a parent in
-- use without children (an empty list). One statement answers both.

-- name: ListCountries :many
SELECT id, name
FROM countries
WHERE retired_at IS NULL
ORDER BY name, id;

-- name: ListCities :many
SELECT c.id, c.name
FROM countries p
LEFT JOIN cities c ON c.country_id = p.id AND c.retired_at IS NULL
WHERE p.id = sqlc.arg(country_id) AND p.retired_at IS NULL
ORDER BY c.name, c.id;

-- name: ListDistricts :many
SELECT c.id, c.name
FROM cities p
LEFT JOIN districts c ON c.city_id = p.id AND c.retired_at IS NULL
WHERE p.id = sqlc.arg(city_id) AND p.retired_at IS NULL
ORDER BY c.name, c.id;

-- name: ListUniversities :many
SELECT id, name
FROM universities
WHERE retired_at IS NULL
ORDER BY name, id;

-- name: ListFaculties :many
SELECT c.id, c.name
FROM universities p
LEFT JOIN faculties c ON c.university_id = p.id AND c.retired_at IS NULL
WHERE p.id = sqlc.arg(university_id) AND p.retired_at IS NULL
ORDER BY c.name, c.id;

-- name: ListDepartments :many
SELECT c.id, c.name
FROM faculties p
LEFT JOIN departments c ON c.faculty_id = p.id AND c.retired_at IS NULL
WHERE p.id = sqlc.arg(faculty_id) AND p.retired_at IS NULL
ORDER BY c.name, c.id;

-- name: GetDataVersion :one
-- GetDataVersion is the data version the tables hold and its revision,
-- both 0 before the first sync. The lists' ETag derives from them.
SELECT coalesce(max(version), 0)::integer AS version, coalesce(max(revision), 0)::integer AS revision
FROM reference_data_version;
