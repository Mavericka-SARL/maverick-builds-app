-- Who may delete a table's records. delete_access '' keeps the rule as it
-- was: whoever may change a record may delete it. 'admins': the
-- application's administrators, and those who may change a record and hold
-- one of delete_roles (role names, matched as create_roles are). Asked
-- 2026-11-20 for a help desk, whose tickets are closed, not deleted.
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS delete_access text NOT NULL DEFAULT ''
    CHECK (delete_access IN ('', 'admins'));
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS delete_roles jsonb NOT NULL DEFAULT '[]'::jsonb;
