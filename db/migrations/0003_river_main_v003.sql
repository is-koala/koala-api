-- River main migration line, version 3 (003_river_job_tags_non_null), as printed by
-- `river migrate-get --line main --version 3`, copied at riverpgxv5 v0.49.0.
-- Do not edit: TestRiverMigrationsMatchRiver compares this file with the
-- SQL of the River version in go.mod. One River version per goose file, so
-- each runs in its own transaction (ADR-0017).

-- +goose Up
-- +goose StatementBegin
ALTER TABLE river_job ALTER COLUMN tags SET DEFAULT '{}';
UPDATE river_job SET tags = '{}' WHERE tags IS NULL;
ALTER TABLE river_job ALTER COLUMN tags SET NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE river_job
    ALTER COLUMN tags DROP NOT NULL,
    ALTER COLUMN tags DROP DEFAULT;

-- +goose StatementEnd
