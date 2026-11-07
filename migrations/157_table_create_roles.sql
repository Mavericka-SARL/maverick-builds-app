-- Who may add a table's records: role names, matched as a workflow's
-- start_roles and a step's assignee_roles are (internal/workflow/assignee).
-- Empty, anyone who reaches the application may, as before; the
-- application's administrators always may. Found 2026-11-06: any app user
-- could add a plant or a GL account, each a new dimension member, without
-- the page that holds the table.
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS create_roles jsonb NOT NULL DEFAULT '[]'::jsonb;
