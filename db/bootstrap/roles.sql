-- Cluster-level group roles (ADR-0013, ADR-0017). Roles belong to the
-- Postgres cluster, not to a database, so they live outside goose's
-- per-database history. `koala bootstrap` runs this in every release,
-- before `koala migrate up` (scripts/release-db.sh). It is idempotent and
-- safe to run concurrently.
--
--   koala_owner  owns every schema object; migrations run as this role.
--   koala_app    the application; only active_<t> views and River objects.
--   koala_maint  restore, cleanup and anonymization on the base tables.
--
-- They are group roles: they hold privileges and cannot log in. A group
-- role found with LOGIN is switched to NOLOGIN. Login users are created by
-- the operator with passwords from the secret store, each a member of
-- exactly one group role (docs/runbook/veritabani.md).

DO $$
DECLARE
    r text;
BEGIN
    -- Two releases bootstrapping at once must not both try CREATE ROLE.
    PERFORM pg_advisory_xact_lock(hashtext('koala bootstrap roles'));

    FOREACH r IN ARRAY ARRAY['koala_owner', 'koala_app', 'koala_maint'] LOOP
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = r) THEN
            BEGIN
                EXECUTE format('CREATE ROLE %I NOLOGIN', r);
            EXCEPTION WHEN duplicate_object THEN
                -- Created meanwhile by a session that does not take the
                -- lock (an operator by hand); nothing to do.
                NULL;
            END;
        END IF;
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = r AND rolcanlogin) THEN
            RAISE NOTICE 'role % had LOGIN; setting NOLOGIN', r;
            EXECUTE format('ALTER ROLE %I NOLOGIN', r);
        END IF;
    END LOOP;
END
$$;
