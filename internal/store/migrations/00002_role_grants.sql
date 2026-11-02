-- Grants for the login roles. Requires blackbox_app and blackbox_reader to exist
-- (scripts/db/setup.sql, since roles are server-wide).
--
-- blackbox_app:    the collector: reads and writes the store, deletes what's
--                  past retention; reads the migrations table.
-- blackbox_reader: the API and pgAdmin: reads everything, including tables
--                  added later.

-- +goose Up
REVOKE ALL ON SCHEMA blackbox FROM PUBLIC;
GRANT USAGE ON SCHEMA blackbox TO blackbox_app, blackbox_reader;

GRANT SELECT, INSERT, UPDATE, DELETE ON
    blackbox.containers, blackbox.records, blackbox.gaps, blackbox.collector_status
    TO blackbox_app;
GRANT SELECT ON blackbox.goose_db_version TO blackbox_app;

GRANT SELECT ON ALL TABLES IN SCHEMA blackbox TO blackbox_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE blackbox_owner IN SCHEMA blackbox
    GRANT SELECT ON TABLES TO blackbox_reader;
