-- Mirrors the columns of migrations/200_record_person_editors.sql for this package's test schema.
ALTER TABLE model.form_def
    ADD COLUMN IF NOT EXISTS editor_field text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS editor_roles text[] NOT NULL DEFAULT '{}';
