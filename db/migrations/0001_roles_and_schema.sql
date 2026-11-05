-- Role model of ADR-0013 and ADR-0017. The group roles come from
-- db/bootstrap/roles.sql; this migration checks for them and closes the
-- database and the public schema so that a role has nothing it was not
-- granted explicitly.

-- +goose Up
-- +goose StatementBegin
DO $$
DECLARE
    missing text;
BEGIN
    SELECT string_agg(r, ', ') INTO missing
    FROM unnest(ARRAY['koala_owner', 'koala_app', 'koala_maint']) AS r
    WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = r);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'missing roles: %', missing
            USING HINT = 'run koala bootstrap (db/bootstrap/roles.sql) first';
    END IF;

    -- Objects are owned by whoever creates them. Running as anyone else
    -- (a superuser, the login user) would leave objects koala_owner cannot
    -- alter in the next migration.
    IF current_user <> 'koala_owner' THEN
        RAISE EXCEPTION 'migrations must run as koala_owner, not %', current_user
            USING HINT = 'koala migrate runs SET ROLE koala_owner; the login must be a member';
    END IF;

    -- Every database starts with CONNECT and TEMPORARY for PUBLIC. Only the
    -- group roles connect; nobody needs temporary tables. GRANT and REVOKE
    -- on a database need its name, hence the dynamic SQL.
    EXECUTE format('REVOKE CONNECT, TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO koala_app, koala_maint', current_database());
END
$$;
-- +goose StatementEnd

REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO koala_app, koala_maint;

-- Functions are executable by PUBLIC by default. Functions koala_owner
-- creates from now on are not; each grant is explicit.
ALTER DEFAULT PRIVILEGES FOR ROLE koala_owner REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

-- +goose Down
ALTER DEFAULT PRIVILEGES FOR ROLE koala_owner GRANT EXECUTE ON FUNCTIONS TO PUBLIC;
REVOKE USAGE ON SCHEMA public FROM koala_app, koala_maint;
GRANT USAGE ON SCHEMA public TO PUBLIC;

-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('REVOKE CONNECT ON DATABASE %I FROM koala_app, koala_maint', current_database());
    EXECUTE format('GRANT CONNECT, TEMPORARY ON DATABASE %I TO PUBLIC', current_database());
END
$$;
-- +goose StatementEnd
