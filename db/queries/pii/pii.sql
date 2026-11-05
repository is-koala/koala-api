-- name: InsertPIIAccess :exec
-- InsertPIIAccess records one PII grant in the reading transaction
-- (pii.Gate.Open); when it fails there is no grant.
INSERT INTO pii_access_log (id, created_at, staff_id, basis, data_subject_id, purpose, fields, request_id)
VALUES (@id, @created_at, @staff_id, @basis, @data_subject_id, @purpose, @fields::text[], sqlc.narg(request_id));
