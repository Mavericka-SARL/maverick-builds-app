-- What a line of Recent Imports says about an import beyond its counts: what
-- it wrote into (a grid, a dimension or a table of records, named as it was
-- then), the grid import's mode, and the person's file.
--
-- Reported 2026-10-23 by a self-hosting tester: Recent Imports showed a date,
-- a count and a status, so which file went where, and how, could be read only
-- in the audit log, which a developer without the tenant administrator's role
-- cannot open. A dimension's members and a table's records left no line at
-- all; they are recorded as jobs from now on, with these columns filled.
-- A job from before keeps empty strings here and shows as before.
ALTER TABLE import.import_job
    ADD COLUMN target_type text NOT NULL DEFAULT '',
    ADD COLUMN target_name text NOT NULL DEFAULT '',
    ADD COLUMN import_mode text NOT NULL DEFAULT '',
    ADD COLUMN file_name   text NOT NULL DEFAULT '';
