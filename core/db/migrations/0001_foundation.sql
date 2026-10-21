-- 0001 foundation: application schema and baseline privileges.
-- Product tables arrive with I02/I03. Every later table receives explicit
-- per-role grants in its own migration; no default privileges are set, so a
-- new table is inaccessible to executor and observer until granted.

REVOKE CREATE ON SCHEMA public FROM PUBLIC;

CREATE SCHEMA steelmatter;
REVOKE ALL ON SCHEMA steelmatter FROM PUBLIC;
GRANT USAGE ON SCHEMA steelmatter TO steelmatter_executor, steelmatter_observer;

CREATE TABLE steelmatter.schema_info (
    key   text PRIMARY KEY,
    value text NOT NULL
);

INSERT INTO steelmatter.schema_info (key, value) VALUES
    ('baseline', '0.6'),
    ('packet_revision', '0.6.2');

REVOKE ALL ON steelmatter.schema_info FROM PUBLIC;
GRANT SELECT ON steelmatter.schema_info TO steelmatter_executor, steelmatter_observer;
