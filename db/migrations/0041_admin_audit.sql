-- The staff audit trail (plan §5.5, ADR-0026; Java admin_audit_entries,
-- N01 §4.4) and the PII access log (plan §5.3, §5.5).
--
-- admin_audit_entries is append-only evidence (M1). A mutation writes a
-- PENDING row before it acts and a result row (SUCCEEDED or FAILED) that
-- names the pending row after it (core-backend's team audit pattern): a
-- pending row without a result is an attempt whose outcome is unknown. A
-- refusal is one DENIED row, a sensitive read one SUCCEEDED or FAILED row
-- without a pending one. The row carries the actor, the basis of the
-- authorization decision (authz.Basis, "… > staff_role:<id>#assignee@…"),
-- the target, the action, the request id and the outcome.
--
-- Retention (db/softdelete.yaml, ADR-0026): an entry 24 months, a
-- FINANCIAL one 10 years; the details (masked body ≤ 8 KB, client
-- address) 6 months; the PII access log 24 months. The database stamps
-- every row: created_at is the statement's time whatever the writer
-- sends, and retain_until follows from it, so no client clock moves a
-- retention; a result row keeps its pending row's. The stamp functions
-- run as the owner (SECURITY DEFINER), so the application needs no read
-- access to the trail. Rows are never updated; only koala_maint deletes
-- them, and only after retain_until (forbid_evidence_change, 0040).

-- +goose Up
CREATE TABLE admin_audit_entries (
    id uuid PRIMARY KEY,
    created_at timestamptz NOT NULL,
    outcome text NOT NULL,
    -- The PENDING row a SUCCEEDED or FAILED row of a mutation closes.
    result_of uuid REFERENCES admin_audit_entries (id),
    actor_kind text NOT NULL,
    actor_id uuid,
    action text NOT NULL,
    permission text,
    basis text,
    target_type text,
    target_id text,
    request_id text,
    mutating boolean NOT NULL,
    sensitivity text NOT NULL,
    retain_until timestamptz NOT NULL,
    CONSTRAINT admin_audit_entries_outcome_check CHECK (outcome IN ('PENDING', 'SUCCEEDED', 'FAILED', 'DENIED')),
    CONSTRAINT admin_audit_entries_actor_kind_check CHECK (actor_kind IN ('staff', 'system')),
    CONSTRAINT admin_audit_entries_actor_id_check CHECK ((actor_kind = 'system') = (actor_id IS NULL)),
    CONSTRAINT admin_audit_entries_action_check CHECK (action ~ '^[A-Za-z][A-Za-z0-9_.]{0,127}$'),
    CONSTRAINT admin_audit_entries_permission_check CHECK (permission ~ '^[a-z][a-z_]*(\.[a-z][a-z_]*)+$'),
    CONSTRAINT admin_audit_entries_basis_check CHECK (length(basis) BETWEEN 1 AND 4096),
    CONSTRAINT admin_audit_entries_target_check CHECK ((target_type IS NULL) = (target_id IS NULL)),
    CONSTRAINT admin_audit_entries_target_type_check CHECK (target_type ~ '^[a-z][a-z_]{0,62}$'),
    CONSTRAINT admin_audit_entries_target_id_check CHECK (length(target_id) BETWEEN 1 AND 256),
    CONSTRAINT admin_audit_entries_request_id_check CHECK (length(request_id) BETWEEN 1 AND 128),
    CONSTRAINT admin_audit_entries_sensitivity_check CHECK (sensitivity IN ('NORMAL', 'PERSONAL_DATA', 'FINANCIAL')),
    -- Only a mutation has a pending row; a refusal decided nothing.
    CONSTRAINT admin_audit_entries_pending_check CHECK (outcome <> 'PENDING' OR mutating),
    CONSTRAINT admin_audit_entries_denied_check CHECK (outcome <> 'DENIED' OR basis IS NULL),
    -- The result of a mutation names its pending row; nothing else does.
    CONSTRAINT admin_audit_entries_result_check CHECK (
        (result_of IS NOT NULL) = (mutating AND outcome IN ('SUCCEEDED', 'FAILED')))
);

-- A pending row has at most one result.
CREATE UNIQUE INDEX admin_audit_entries_result_of_key ON admin_audit_entries (result_of) WHERE result_of IS NOT NULL;
CREATE INDEX admin_audit_entries_retain_until_idx ON admin_audit_entries (retain_until);
CREATE INDEX admin_audit_entries_created_at_idx ON admin_audit_entries (created_at);

-- The part of an entry that holds personal data: the request body with
-- its personal data masked (≤ 8 KB) and the client address. Kept 6 months.
CREATE TABLE admin_audit_entry_details (
    entry_id uuid PRIMARY KEY REFERENCES admin_audit_entries (id),
    created_at timestamptz NOT NULL,
    body jsonb,
    client_key text,
    retain_until timestamptz NOT NULL,
    -- audit.MaskBody caps the compact JSON at 8 KB; jsonb's text form
    -- adds a space after every separator, hence the margin.
    CONSTRAINT admin_audit_entry_details_body_check CHECK (octet_length(body::text) <= 16384),
    CONSTRAINT admin_audit_entry_details_client_key_check CHECK (length(client_key) BETWEEN 1 AND 64)
);

CREATE INDEX admin_audit_entry_details_retain_until_idx ON admin_audit_entry_details (retain_until);

-- One row per PII grant (pii.Gate.Open), written in the transaction that
-- reads the data: no row, no grant (plan §5.5).
CREATE TABLE pii_access_log (
    id uuid PRIMARY KEY,
    created_at timestamptz NOT NULL,
    staff_id uuid NOT NULL,
    basis text NOT NULL,
    data_subject_id uuid NOT NULL,
    purpose text NOT NULL,
    fields text[] NOT NULL,
    -- The operation that read the data, "GET /v1/admin/users/{userId}"
    -- (plan §5.5: "uç").
    route text,
    request_id text,
    retain_until timestamptz NOT NULL,
    CONSTRAINT pii_access_log_basis_check CHECK (length(basis) BETWEEN 1 AND 4096),
    CONSTRAINT pii_access_log_purpose_check CHECK (purpose ~ '^[a-z][a-z_]{0,62}$'),
    CONSTRAINT pii_access_log_fields_check CHECK (cardinality(fields) BETWEEN 1 AND 32),
    CONSTRAINT pii_access_log_route_check CHECK (length(route) BETWEEN 1 AND 256),
    CONSTRAINT pii_access_log_request_id_check CHECK (length(request_id) BETWEEN 1 AND 128)
);

CREATE INDEX pii_access_log_retain_until_idx ON pii_access_log (retain_until);
CREATE INDEX pii_access_log_data_subject_idx ON pii_access_log (data_subject_id, created_at);

-- +goose StatementBegin
-- retain_until of an audit entry: a result row keeps its pending row's
-- (and must close a pending row of the same actor and action); any other
-- row is kept 24 months, 10 years when FINANCIAL.
CREATE FUNCTION admin_audit_entries_stamp() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    pending admin_audit_entries%ROWTYPE;
BEGIN
    NEW.created_at := statement_timestamp();
    IF NEW.result_of IS NOT NULL THEN
        SELECT * INTO pending FROM admin_audit_entries WHERE id = NEW.result_of;
        IF NOT FOUND OR pending.outcome <> 'PENDING'
           OR pending.actor_kind <> NEW.actor_kind
           OR pending.actor_id IS DISTINCT FROM NEW.actor_id
           OR pending.action <> NEW.action THEN
            RAISE EXCEPTION 'result_of must name a pending entry of the same actor and action'
                USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'admin_audit_entries_result_of_pending';
        END IF;
        NEW.retain_until := pending.retain_until;
    ELSIF NEW.sensitivity = 'FINANCIAL' THEN
        NEW.retain_until := NEW.created_at + interval '10 years';
    ELSE
        NEW.retain_until := NEW.created_at + interval '24 months';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION admin_audit_entry_details_stamp() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
BEGIN
    NEW.created_at := statement_timestamp();
    NEW.retain_until := NEW.created_at + interval '6 months';
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION pii_access_log_stamp() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
BEGIN
    NEW.created_at := statement_timestamp();
    NEW.retain_until := NEW.created_at + interval '24 months';
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER admin_audit_entries_stamp BEFORE INSERT ON admin_audit_entries
    FOR EACH ROW EXECUTE FUNCTION admin_audit_entries_stamp();
CREATE TRIGGER admin_audit_entry_details_stamp BEFORE INSERT ON admin_audit_entry_details
    FOR EACH ROW EXECUTE FUNCTION admin_audit_entry_details_stamp();
CREATE TRIGGER pii_access_log_stamp BEFORE INSERT ON pii_access_log
    FOR EACH ROW EXECUTE FUNCTION pii_access_log_stamp();

CREATE TRIGGER admin_audit_entries_immutable BEFORE UPDATE OR DELETE ON admin_audit_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_evidence_change();
CREATE TRIGGER admin_audit_entries_no_truncate BEFORE TRUNCATE ON admin_audit_entries
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_evidence_change();
CREATE TRIGGER admin_audit_entry_details_immutable BEFORE UPDATE OR DELETE ON admin_audit_entry_details
    FOR EACH ROW EXECUTE FUNCTION forbid_evidence_change();
CREATE TRIGGER admin_audit_entry_details_no_truncate BEFORE TRUNCATE ON admin_audit_entry_details
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_evidence_change();
CREATE TRIGGER pii_access_log_immutable BEFORE UPDATE OR DELETE ON pii_access_log
    FOR EACH ROW EXECUTE FUNCTION forbid_evidence_change();
CREATE TRIGGER pii_access_log_no_truncate BEFORE TRUNCATE ON pii_access_log
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_evidence_change();

-- The application only appends (the stamp trigger reads the pending row
-- as the owner); reading the trail is audit.read's, through staff
-- management (Faz 8). The `koala staff` command writes as koala_maint,
-- which also purges after the retention.
GRANT INSERT ON admin_audit_entries, admin_audit_entry_details, pii_access_log TO koala_app;
GRANT SELECT, INSERT, DELETE ON admin_audit_entries, admin_audit_entry_details TO koala_maint;
GRANT SELECT, DELETE ON pii_access_log TO koala_maint;

-- +goose Down
DROP TABLE pii_access_log;
DROP TABLE admin_audit_entry_details;
DROP TABLE admin_audit_entries;
DROP FUNCTION pii_access_log_stamp();
DROP FUNCTION admin_audit_entry_details_stamp();
DROP FUNCTION admin_audit_entries_stamp();
