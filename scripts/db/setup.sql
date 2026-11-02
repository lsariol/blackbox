-- Blackbox database: the one-time Admin setup on sparkdb.
--
-- Creates what's missing and fixes ownership and access:
--   roles     blackbox_owner (NOLOGIN), blackbox_migrator (member of the owner,
--             acting as it), blackbox_app, blackbox_reader
--   database  blackbox_db, owned by blackbox_owner; only the three login roles may
--             connect
--   schema    blackbox, if it exists already: owned by blackbox_owner
-- Everything inside the schema (tables, grants) comes from Blackbox's goose
-- migrations, never from here.
--
-- Safe to run again. It never changes the password of a role that exists; a
-- password is only needed for a role that doesn't exist yet, and every missing
-- one is checked before anything changes.
--
-- Run as the Admin superuser (on Spark, over SSH):
--   docker exec -i sparkdb psql -U Admin -d postgres \
--     -v migrator_password="..." -v app_password="..." -v reader_password="..." \
--     -f - < scripts/db/setup.sql

\set ON_ERROR_STOP on
SET client_min_messages = warning;

SELECT NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'blackbox_owner')    AS need_owner,
       NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'blackbox_migrator') AS need_migrator,
       NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'blackbox_app')      AS need_app,
       NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'blackbox_reader')   AS need_reader,
       NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'blackbox_db')    AS need_db,
       current_setting('is_superuser') = 'on' AS is_superuser
\gset

-- 1. Preconditions, before anything changes.

\if :is_superuser
\else
DO $$ BEGIN RAISE EXCEPTION 'Run this as the Admin superuser (psql -U Admin).'; END $$;
\endif

\if :need_migrator
\if :{?migrator_password}
\else
DO $$ BEGIN RAISE EXCEPTION 'blackbox_migrator doesn''t exist: pass its password with -v migrator_password=... Nothing was changed.'; END $$;
\endif
\endif

\if :need_app
\if :{?app_password}
\else
DO $$ BEGIN RAISE EXCEPTION 'blackbox_app doesn''t exist: pass its password with -v app_password=... Nothing was changed.'; END $$;
\endif
\endif

\if :need_reader
\if :{?reader_password}
\else
DO $$ BEGIN RAISE EXCEPTION 'blackbox_reader doesn''t exist: pass its password with -v reader_password=... Nothing was changed.'; END $$;
\endif
\endif

-- 2. Roles.

\if :need_owner
\echo 'Creating blackbox_owner'
CREATE ROLE blackbox_owner NOLOGIN;
\else
-- Nothing logs in as the owner; its old password (if any) goes too.
ALTER ROLE blackbox_owner NOLOGIN PASSWORD NULL;
\endif

\if :need_migrator
\echo 'Creating blackbox_migrator'
CREATE ROLE blackbox_migrator LOGIN PASSWORD :'migrator_password';
\else
ALTER ROLE blackbox_migrator LOGIN;
\endif

\if :need_app
\echo 'Creating blackbox_app'
CREATE ROLE blackbox_app LOGIN PASSWORD :'app_password';
\else
ALTER ROLE blackbox_app LOGIN;
\endif

\if :need_reader
\echo 'Creating blackbox_reader'
CREATE ROLE blackbox_reader LOGIN PASSWORD :'reader_password';
\else
ALTER ROLE blackbox_reader LOGIN;
\endif

-- The migrator acts as the owner, so everything the migrations create is
-- owned by blackbox_owner, not by whoever ran them.
GRANT blackbox_owner TO blackbox_migrator;
ALTER ROLE blackbox_migrator SET role = 'blackbox_owner';

-- 3. The database.

\if :need_db
\echo 'Creating blackbox_db'
CREATE DATABASE blackbox_db OWNER blackbox_owner;
\else
ALTER DATABASE blackbox_db OWNER TO blackbox_owner;
\endif

REVOKE ALL ON DATABASE blackbox_db FROM PUBLIC;
GRANT CONNECT ON DATABASE blackbox_db TO blackbox_migrator, blackbox_app, blackbox_reader;

-- 4. Inside the database: the blackbox schema (if an earlier setup made it)
-- belongs to the owner, and nobody else may create objects in public.

\connect blackbox_db
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'blackbox') THEN
    ALTER SCHEMA blackbox OWNER TO blackbox_owner;
  END IF;
END $$;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

\echo
\echo 'Done. Check: every row below matches its "want".'
\echo
\connect postgres
SELECT r.rolname AS role, r.rolcanlogin AS can_login,
       coalesce((SELECT string_agg(b.rolname, ', ') FROM pg_auth_members m JOIN pg_roles b ON b.oid = m.roleid
                  WHERE m.member = r.oid), '') AS member_of,
       coalesce(array_to_string(r.rolconfig, '; '), '') AS settings,
       CASE r.rolname
         WHEN 'blackbox_owner'    THEN 'NOLOGIN'
         WHEN 'blackbox_migrator' THEN 'LOGIN, member of blackbox_owner, role=blackbox_owner'
         ELSE 'LOGIN'
       END AS want
  FROM pg_roles r
 WHERE r.rolname IN ('blackbox_owner', 'blackbox_migrator', 'blackbox_app', 'blackbox_reader')
 ORDER BY r.rolname;
SELECT datname AS database, pg_get_userbyid(datdba) AS owner, array_to_string(datacl, ' ') AS access,
       'owner blackbox_owner; c (connect) for migrator, app, reader' AS want
  FROM pg_database WHERE datname = 'blackbox_db';
