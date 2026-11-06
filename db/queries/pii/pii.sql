-- name: InsertPIIAccess :exec
-- InsertPIIAccess records one PII grant in the reading transaction
-- (pii.Gate.Open); when it fails there is no grant.
-- The database stamps created_at and retain_until (0041).
INSERT INTO pii_access_log (id, created_at, staff_id, basis, data_subject_id, purpose, fields, route, request_id)
VALUES (@id, now(), @staff_id, @basis, @data_subject_id, @purpose, @fields::text[], sqlc.narg(route), sqlc.narg(request_id));
