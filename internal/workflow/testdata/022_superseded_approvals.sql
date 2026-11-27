-- Mirrors migrations/201_superseded_approvals.sql for this package's test schema.
ALTER TABLE workflow.workflow_instance
    ADD COLUMN IF NOT EXISTS superseded_by uuid REFERENCES workflow.workflow_instance(id) ON DELETE SET NULL;
