-- River tables are exemption class M3 (ADR-0013): River manages them and
-- the application role works on them directly. The objects are owned by
-- koala_owner, so koala_app gets exactly what River's client needs: DML on
-- its tables, the sequences behind their bigserial ids and River's
-- function. No DDL: River's in-process reindexer is off; `koala maint
-- reindex` rebuilds the indexes as koala_owner (ADR-0017).
--
-- A River upgrade that adds a table or function needs a grant here (in a
-- new migration); TestRoleGrants fails until it has one.

-- +goose Up
GRANT SELECT, INSERT, UPDATE, DELETE
    ON river_job, river_leader, river_queue, river_notification
    TO koala_app;
GRANT USAGE, SELECT
    ON SEQUENCE river_job_id_seq, river_notification_id_seq
    TO koala_app;
GRANT EXECUTE
    ON FUNCTION river_job_state_in_bitmask(bit, river_job_state)
    TO koala_app;

-- +goose Down
REVOKE ALL
    ON FUNCTION river_job_state_in_bitmask(bit, river_job_state)
    FROM koala_app;
REVOKE ALL
    ON SEQUENCE river_job_id_seq, river_notification_id_seq
    FROM koala_app;
REVOKE ALL
    ON river_job, river_leader, river_queue, river_notification
    FROM koala_app;
