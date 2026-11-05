-- River main migration line, version 8 (008_job_id_autoincrement), as printed by
-- `river migrate-get --line main --version 8`, copied at riverpgxv5 v0.49.0.
-- Do not edit: TestRiverMigrationsMatchRiver compares this file with the
-- SQL of the River version in go.mod. One River version per goose file, so
-- each runs in its own transaction (ADR-0017).

-- +goose Up
-- +goose StatementBegin
-- No-op. PostgreSQL sequences already prevent automatically generated job IDs
-- from being reused.
SELECT 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op. PostgreSQL sequences already prevent automatically generated job IDs
-- from being reused.
SELECT 1;

-- +goose StatementEnd
