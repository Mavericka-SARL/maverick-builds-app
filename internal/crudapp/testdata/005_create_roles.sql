-- Test-schema mirror of migrations/157_table_create_roles.sql: who may add a
-- table's records, by role name.
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS create_roles jsonb NOT NULL DEFAULT '[]'::jsonb;
