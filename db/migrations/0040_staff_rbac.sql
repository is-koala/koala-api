-- Staff RBAC (ADR-0014, ADR-0026; plan §5.5): roles, the role inclusion
-- DAG, role grants of the 25 staff permissions and role assignments. The
-- permissions themselves are defined by the code that checks them
-- (internal/authz/permissions.go, one platform relation of model.fga
-- each); the database keeps the grants and refuses an unknown code.
--
-- Every request resolves the caller's permissions from these tables with
-- one recursive query (internal/authz/authzsql), so a revoked assignment
-- or a removed inclusion takes effect on the next request (AUTH-012).
--
-- Invariants kept by the database, whoever writes:
--   - the inclusion graph is acyclic (staff_role_includes_acyclic);
--   - separation of duties (staff_separation_of_duties): no role that is
--     not exempt reaches both the TRUST and the FINANCE branch, and no
--     staff member without an exempt role does (Java
--     StaffSeparationOfDuties: OWNER is the exemption). A retired role
--     exempts nobody. Inserts, updates and deletes are all checked: taking
--     OWNER away from a member who holds both branches is refused too;
--   - disabling a staff member moves its token_version, so every token of
--     the account dies with it (ADR-0025 §6).
-- Writers of the graph (insert, update, delete) serialize on one advisory
-- lock, so two concurrent writes cannot each pass the check and together
-- break it.
--
-- Classes (db/softdelete.yaml): staff_roles is reference data (M4, retired
-- with retired_at, never deleted: audit rows name roles by id); grants,
-- inclusions and assignments are links (M6, hard-deleted on revoke; the
-- change is recorded in authz_change_log). authz_change_log is evidence
-- (M1).

-- +goose Up
CREATE TABLE staff_roles (
    id uuid PRIMARY KEY,
    code text NOT NULL,
    name text NOT NULL,
    -- TRUST or FINANCE for the two branches separation of duties keeps
    -- apart; NULL for the rest.
    duty_branch text,
    -- A role whose holder may span both branches (OWNER).
    separation_exempt boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    retired_at timestamptz,
    CONSTRAINT staff_roles_code_check CHECK (code ~ '^[A-Z][A-Z_]{1,63}$'),
    CONSTRAINT staff_roles_name_check CHECK (length(name) BETWEEN 1 AND 160),
    CONSTRAINT staff_roles_duty_branch_check CHECK (duty_branch IN ('TRUST', 'FINANCE')),
    CONSTRAINT staff_roles_retired_after_created CHECK (retired_at >= created_at),
    CONSTRAINT staff_roles_code_key UNIQUE (code)
);

-- A role's grant of one permission. The CHECK is the permission catalog of
-- internal/authz/permissions.go; TestPermissionCatalogMatchesDatabase
-- keeps the two equal.
CREATE TABLE staff_role_permissions (
    role_id uuid NOT NULL REFERENCES staff_roles (id),
    permission text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (role_id, permission),
    CONSTRAINT staff_role_permissions_permission_check CHECK (permission IN (
        'audit.read', 'badge.manage', 'bank.account.verify', 'category.manage', 'company.approve',
        'content.moderate', 'dispute.resolve', 'finance.read', 'flag.manage', 'identity.review',
        'job.read', 'note.read', 'note.write', 'payment.read', 'payment.refund',
        'payout.reconcile', 'payout.release', 'role.manage', 'settings.manage', 'staff.manage',
        'staff.read', 'user.deletion.manage', 'user.read', 'user.read_pii', 'user.role.manage'))
);

-- parent includes child: whoever holds parent holds child's permissions
-- (model.fga: staff_role:<child>#includer@staff_role:<parent>).
CREATE TABLE staff_role_includes (
    parent_role_id uuid NOT NULL REFERENCES staff_roles (id),
    child_role_id uuid NOT NULL REFERENCES staff_roles (id),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (parent_role_id, child_role_id),
    CONSTRAINT staff_role_includes_no_self_edge CHECK (parent_role_id <> child_role_id)
);

CREATE INDEX staff_role_includes_child_idx ON staff_role_includes (child_role_id);

CREATE TABLE staff_role_assignments (
    staff_id uuid NOT NULL REFERENCES staff_members (id),
    role_id uuid NOT NULL REFERENCES staff_roles (id),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (staff_id, role_id)
);

CREATE INDEX staff_role_assignments_role_idx ON staff_role_assignments (role_id);

-- One row per relationship change (plan §5.2-5, ADR-0024 §10): the tuple
-- in OpenFGA form, written by authz.RelationSink in the writer's
-- transaction. Evidence (M1): never updated or deleted.
CREATE TABLE authz_change_log (
    id uuid PRIMARY KEY,
    created_at timestamptz NOT NULL,
    op text NOT NULL,
    tuple text NOT NULL,
    actor_kind text NOT NULL,
    actor_id uuid,
    request_id text,
    CONSTRAINT authz_change_log_op_check CHECK (op IN ('WRITE', 'DELETE')),
    CONSTRAINT authz_change_log_tuple_check CHECK (length(tuple) BETWEEN 5 AND 1024),
    CONSTRAINT authz_change_log_actor_kind_check CHECK (actor_kind IN ('staff', 'system')),
    CONSTRAINT authz_change_log_actor_id_check CHECK ((actor_kind = 'system') = (actor_id IS NULL)),
    CONSTRAINT authz_change_log_request_id_check CHECK (length(request_id) BETWEEN 1 AND 128)
);

-- +goose StatementBegin
-- Serializes the writers of the role graph (BEFORE … FOR EACH STATEMENT):
-- the check below then sees every committed write of the others.
CREATE FUNCTION staff_rbac_lock() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('koala staff rbac graph'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The invariants of the role graph, checked over the whole graph after
-- every write statement, deletes included (AFTER … FOR EACH STATEMENT). Ten roles and a
-- handful of staff make the full check cheaper than an incremental one,
-- and it cannot miss a path.
CREATE FUNCTION staff_rbac_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    bad text;
BEGIN
    -- A role that reaches itself through inclusions.
    WITH RECURSIVE walk (start_id, role_id, path) AS (
        SELECT parent_role_id, child_role_id, ARRAY[parent_role_id]
        FROM staff_role_includes
      UNION ALL
        SELECT w.start_id, i.child_role_id, w.path || w.role_id
        FROM walk w
        JOIN staff_role_includes i ON i.parent_role_id = w.role_id
        WHERE NOT w.role_id = ANY (w.path)
    )
    SELECT r.code INTO bad
    FROM walk w JOIN staff_roles r ON r.id = w.start_id
    WHERE w.role_id = w.start_id
    LIMIT 1;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'staff role % would include itself', bad
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'staff_role_includes_acyclic';
    END IF;

    -- A role that is not exempt and reaches both branches. Exemption
    -- needs retired_at IS NULL, so a retired exempt role exempts nothing:
    -- OWNER reaches both the TRUST and the FINANCE branch, so once it
    -- exists it can no longer be retired - every
    -- `UPDATE staff_roles SET retired_at = now()` on it is refused, and
    -- so is taking OWNER away from a member who reaches both branches.
    -- That is deliberate and fail-closed (it is the one role that may
    -- cross the branches), but it surprises an operator: to retire OWNER
    -- the branch reach has to go first.
    WITH RECURSIVE closure (root_id, role_id) AS (
        SELECT id, id FROM staff_roles
      UNION
        SELECT c.root_id, i.child_role_id
        FROM closure c
        JOIN staff_role_includes i ON i.parent_role_id = c.role_id
    )
    SELECT root.code INTO bad
    FROM closure c
    JOIN staff_roles root ON root.id = c.root_id
    JOIN staff_roles r ON r.id = c.role_id
    WHERE NOT (root.separation_exempt AND root.retired_at IS NULL)
    GROUP BY root.code
    HAVING bool_or(r.duty_branch = 'TRUST') AND bool_or(r.duty_branch = 'FINANCE')
    LIMIT 1;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'staff role % would reach both the TRUST and the FINANCE branch', bad
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'staff_separation_of_duties';
    END IF;

    -- A staff member without an exempt role who reaches both branches.
    WITH RECURSIVE reach (staff_id, role_id) AS (
        SELECT staff_id, role_id FROM staff_role_assignments
      UNION
        SELECT r.staff_id, i.child_role_id
        FROM reach r
        JOIN staff_role_includes i ON i.parent_role_id = r.role_id
    )
    SELECT reach.staff_id::text INTO bad
    FROM reach JOIN staff_roles r ON r.id = reach.role_id
    GROUP BY reach.staff_id
    HAVING bool_or(r.duty_branch = 'TRUST') AND bool_or(r.duty_branch = 'FINANCE')
       AND NOT EXISTS (
           SELECT FROM staff_role_assignments a JOIN staff_roles x ON x.id = a.role_id
           WHERE a.staff_id = reach.staff_id AND x.separation_exempt AND x.retired_at IS NULL)
    LIMIT 1;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'staff member % would hold roles of both the TRUST and the FINANCE branch', bad
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'staff_separation_of_duties';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER staff_role_includes_lock BEFORE INSERT OR UPDATE OR DELETE ON staff_role_includes
    FOR EACH STATEMENT EXECUTE FUNCTION staff_rbac_lock();
CREATE TRIGGER staff_role_includes_check AFTER INSERT OR UPDATE OR DELETE ON staff_role_includes
    FOR EACH STATEMENT EXECUTE FUNCTION staff_rbac_check();
CREATE TRIGGER staff_role_assignments_lock BEFORE INSERT OR UPDATE OR DELETE ON staff_role_assignments
    FOR EACH STATEMENT EXECUTE FUNCTION staff_rbac_lock();
CREATE TRIGGER staff_role_assignments_check AFTER INSERT OR UPDATE OR DELETE ON staff_role_assignments
    FOR EACH STATEMENT EXECUTE FUNCTION staff_rbac_check();
CREATE TRIGGER staff_roles_lock BEFORE INSERT OR UPDATE OR DELETE ON staff_roles
    FOR EACH STATEMENT EXECUTE FUNCTION staff_rbac_lock();
CREATE TRIGGER staff_roles_check AFTER INSERT OR UPDATE OR DELETE ON staff_roles
    FOR EACH STATEMENT EXECUTE FUNCTION staff_rbac_check();

-- +goose StatementBegin
-- Disabling an account ends every token of it (ADR-0025 §6):
-- an update that takes a member out of ACTIVE without moving its
-- token_version gets the move here.
CREATE FUNCTION staff_members_bump_token_version() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.token_version := OLD.token_version + 1;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER staff_members_disable_bumps_token_version BEFORE UPDATE ON staff_members
    FOR EACH ROW
    WHEN (OLD.status = 'ACTIVE' AND NEW.status <> 'ACTIVE' AND NEW.token_version <= OLD.token_version)
    EXECUTE FUNCTION staff_members_bump_token_version();

-- +goose StatementBegin
-- Evidence rows (M1) are never updated. A row with a retention
-- (retain_until) may be deleted by koala_maint once the retention has
-- passed (ADR-0026); nobody else deletes, and nobody truncates.
CREATE FUNCTION forbid_evidence_change() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    keep_until timestamptz;
BEGIN
    IF TG_OP = 'DELETE' THEN
        keep_until := (to_jsonb(OLD) ->> 'retain_until')::timestamptz;
        IF current_user = 'koala_maint' AND keep_until IS NOT NULL AND keep_until <= now() THEN
            RETURN OLD;
        END IF;
    END IF;
    RAISE EXCEPTION '% rows are immutable evidence (%)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = TG_TABLE_NAME || '_immutable';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER authz_change_log_immutable BEFORE UPDATE OR DELETE ON authz_change_log
    FOR EACH ROW EXECUTE FUNCTION forbid_evidence_change();
CREATE TRIGGER authz_change_log_no_truncate BEFORE TRUNCATE ON authz_change_log
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_evidence_change();

-- The seed: Java's role graph (koala-backend changelogs 2026/09 06-20,
-- 06-21, 06-71, 07-02, 08-01, 11-04, 14-01, 15-05), with its role ids.
INSERT INTO staff_roles (id, code, name, duty_branch, separation_exempt, created_at) VALUES
    ('9a1c0f10-0003-4c00-8000-000000000001', 'CATALOG_EDITOR',   'Katalog editörü',     NULL,      false, now()),
    ('9a1c0f10-0003-4c00-8000-000000000002', 'SUPPORT_AGENT',    'Destek görevlisi',    NULL,      false, now()),
    ('9a1c0f10-0003-4c00-8000-000000000003', 'FINANCE_ANALYST',  'Finans analisti',     'FINANCE', false, now()),
    ('9a1c0f10-0003-4c00-8000-000000000004', 'AUDITOR',          'Denetçi',             NULL,      false, now()),
    ('9a1c0f10-0003-4c00-8000-000000000005', 'MODERATOR',        'Moderatör',           'TRUST',   false, now()),
    ('9a1c0f10-0003-4c00-8000-000000000006', 'DISPUTE_HANDLER',  'Uyuşmazlık uzmanı',   'TRUST',   false, now()),
    ('9a1c0f10-0003-4c00-8000-000000000007', 'COMPANY_APPROVER', 'Şirket onaylayıcısı', NULL,      false, now()),
    ('9a1c0f10-0003-4c00-8000-000000000008', 'TRUST_LEAD',       'Güven ekibi lideri',  'TRUST',   false, now()),
    ('9a1c0f10-0003-4c00-8000-000000000009', 'FINANCE_OFFICER',  'Finans yetkilisi',    'FINANCE', false, now()),
    ('9a1c0f10-0003-4c00-8000-000000000010', 'OWNER',            'Sahip',               NULL,      true,  now());

INSERT INTO staff_role_permissions (role_id, permission, created_at)
SELECT r.id, g.permission, now()
FROM (VALUES
    ('CATALOG_EDITOR',   'category.manage'),
    ('CATALOG_EDITOR',   'badge.manage'),
    ('SUPPORT_AGENT',    'user.read'),
    ('SUPPORT_AGENT',    'job.read'),
    ('SUPPORT_AGENT',    'note.read'),
    ('SUPPORT_AGENT',    'note.write'),
    ('MODERATOR',        'content.moderate'),
    ('DISPUTE_HANDLER',  'dispute.resolve'),
    ('COMPANY_APPROVER', 'company.approve'),
    ('TRUST_LEAD',       'user.read_pii'),
    ('TRUST_LEAD',       'user.deletion.manage'),
    ('TRUST_LEAD',       'identity.review'),
    ('TRUST_LEAD',       'flag.manage'),
    ('FINANCE_ANALYST',  'payment.read'),
    ('FINANCE_ANALYST',  'finance.read'),
    ('FINANCE_OFFICER',  'payout.release'),
    ('FINANCE_OFFICER',  'payment.refund'),
    ('FINANCE_OFFICER',  'bank.account.verify'),
    ('FINANCE_OFFICER',  'payout.reconcile'),
    ('AUDITOR',          'audit.read'),
    ('OWNER',            'staff.manage'),
    ('OWNER',            'staff.read'),
    ('OWNER',            'role.manage'),
    ('OWNER',            'user.role.manage'),
    ('OWNER',            'settings.manage')
) AS g (role_code, permission)
JOIN staff_roles r ON r.code = g.role_code;

INSERT INTO staff_role_includes (parent_role_id, child_role_id, created_at)
SELECT parent.id, child.id, now()
FROM (VALUES
    ('MODERATOR',        'SUPPORT_AGENT'),
    ('DISPUTE_HANDLER',  'SUPPORT_AGENT'),
    ('COMPANY_APPROVER', 'SUPPORT_AGENT'),
    ('TRUST_LEAD',       'MODERATOR'),
    ('TRUST_LEAD',       'DISPUTE_HANDLER'),
    ('FINANCE_OFFICER',  'FINANCE_ANALYST'),
    ('OWNER',            'CATALOG_EDITOR'),
    ('OWNER',            'TRUST_LEAD'),
    ('OWNER',            'COMPANY_APPROVER'),
    ('OWNER',            'FINANCE_OFFICER'),
    ('OWNER',            'AUDITOR')
) AS e (parent_code, child_code)
JOIN staff_roles parent ON parent.code = e.parent_code
JOIN staff_roles child ON child.code = e.child_code;

-- The relationship tuples of the staff tables in OpenFGA form (plan
-- §5.2-1, §5.6-5): for the conformance test, debugging and a future
-- export, never for decisions (the hot path is the typed query of
-- internal/authz/authzsql). Retired roles and soft-deleted or disabled
-- staff have no tuples. No application role reads it.
CREATE VIEW authz_tuples AS
    SELECT 'platform'::text AS object_type, 'koala'::text AS object_id,
           replace(p.permission, '.', '_') AS relation,
           'staff_role'::text AS user_type, p.role_id::text AS user_id, 'assignee'::text AS user_relation
    FROM staff_role_permissions p
    JOIN staff_roles r ON r.id = p.role_id
    WHERE r.retired_at IS NULL
  UNION ALL
    SELECT 'staff_role', i.child_role_id::text, 'includer', 'staff_role', i.parent_role_id::text, NULL
    FROM staff_role_includes i
    JOIN staff_roles c ON c.id = i.child_role_id
    JOIN staff_roles p ON p.id = i.parent_role_id
    WHERE c.retired_at IS NULL AND p.retired_at IS NULL
  UNION ALL
    SELECT 'staff_role', a.role_id::text, 'assignee', 'staff', a.staff_id::text, NULL
    FROM staff_role_assignments a
    JOIN staff_roles r ON r.id = a.role_id
    LEFT JOIN active_staff_members m ON m.id = a.staff_id
    WHERE r.retired_at IS NULL AND m.status = 'ACTIVE';

-- The application resolves permissions from the graph; staff management
-- (the `koala staff` command, Faz 8 API) assigns roles as koala_maint.
-- Roles, grants and inclusions change only by migration until role
-- management lands (Faz 8).
GRANT SELECT ON staff_roles, staff_role_permissions, staff_role_includes, staff_role_assignments TO koala_app;
GRANT SELECT ON staff_roles, staff_role_permissions, staff_role_includes TO koala_maint;
GRANT SELECT, INSERT, DELETE ON staff_role_assignments TO koala_maint;
GRANT INSERT ON authz_change_log TO koala_app, koala_maint;

-- +goose Down
DROP TRIGGER staff_members_disable_bumps_token_version ON staff_members;
DROP FUNCTION staff_members_bump_token_version();
DROP VIEW authz_tuples;
DROP TABLE authz_change_log;
DROP TABLE staff_role_assignments;
DROP TABLE staff_role_includes;
DROP TABLE staff_role_permissions;
DROP TABLE staff_roles;
DROP FUNCTION forbid_evidence_change();
DROP FUNCTION staff_rbac_check();
DROP FUNCTION staff_rbac_lock();
