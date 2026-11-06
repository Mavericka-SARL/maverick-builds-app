-- Mirrors migrations/154_workflow_start_roles.sql for this package's test schema.
ALTER TABLE workflow.workflow_def ADD COLUMN IF NOT EXISTS start_roles jsonb NOT NULL DEFAULT '[]'::jsonb;
