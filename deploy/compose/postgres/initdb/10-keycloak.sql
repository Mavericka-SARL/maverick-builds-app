-- Keycloak keeps its realm, its users and their credentials in a database of
-- its own on the same server. The postgres image creates only POSTGRES_DB, and
-- runs this directory only when the data volume is initialised for the first
-- time; docs/SELF_HOSTING.md covers a volume that predates it.
CREATE DATABASE keycloak OWNER mavericks;
