-- Mirrors migrations/201_superseded_approvals.sql for this package's test schema.
ALTER TABLE workflow.workflow_instance
    ADD COLUMN IF NOT EXISTS superseded_by uuid REFERENCES workflow.workflow_instance(id) ON DELETE SET NULL;
-- The lock reads each run's workflow by name (writeguard/supersede.go), which
-- this package's minimal workflow_def did not have.
ALTER TABLE workflow.workflow_def ADD COLUMN IF NOT EXISTS name text NOT NULL DEFAULT '';
