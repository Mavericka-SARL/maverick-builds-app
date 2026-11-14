-- Mirrors migrations/182_workflow_reopen.sql for this package's test schema.
ALTER TABLE workflow.workflow_def ADD COLUMN IF NOT EXISTS reopen_roles jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE workflow.workflow_instance
  ADD COLUMN IF NOT EXISTS reopened_at timestamptz,
  ADD COLUMN IF NOT EXISTS reopened_by uuid,
  ADD COLUMN IF NOT EXISTS reopen_reason text;
CREATE TABLE IF NOT EXISTS workflow.approval_snapshot (
  instance_id uuid NOT NULL REFERENCES workflow.workflow_instance(id) ON DELETE CASCADE,
  revision_id uuid NOT NULL,
  kind        text NOT NULL CHECK (kind IN ('input', 'calc')),
  metric_id   uuid NOT NULL,
  dim_members jsonb NOT NULL,
  value       numeric,
  text_value  text,
  PRIMARY KEY (instance_id, kind, metric_id, dim_members)
);
