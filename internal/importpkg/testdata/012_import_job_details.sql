-- migrations/149_import_job_details.sql, for the store's tests: what a line
-- of Recent Imports says about an import beyond its counts.
ALTER TABLE import.import_job
    ADD COLUMN target_type text NOT NULL DEFAULT '',
    ADD COLUMN target_name text NOT NULL DEFAULT '',
    ADD COLUMN import_mode text NOT NULL DEFAULT '',
    ADD COLUMN file_name   text NOT NULL DEFAULT '';
