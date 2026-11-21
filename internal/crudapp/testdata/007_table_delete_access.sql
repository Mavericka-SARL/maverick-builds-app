-- Mirrors migrations/192_table_delete_access.sql for this package's test schema.
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS delete_access text NOT NULL DEFAULT ''
    CHECK (delete_access IN ('', 'admins'));
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS delete_roles jsonb NOT NULL DEFAULT '[]'::jsonb;
