-- Mirrors migrations/193_table_record_scope_and_codes.sql for this package's test schema.
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS record_scope text NOT NULL DEFAULT ''
    CHECK (record_scope IN ('', 'open'));
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS existing_codes text NOT NULL DEFAULT ''
    CHECK (existing_codes IN ('', 'refuse', 'update'));
