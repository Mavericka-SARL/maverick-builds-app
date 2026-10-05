-- Test-schema mirror of what the write guard's approvals-only workflow lock
-- reads: a step's definition id and status, the definition's steps and the
-- instance's snapshot of them (production: migrations 007 and 075).
ALTER TABLE workflow.workflow_def ADD COLUMN IF NOT EXISTS steps JSONB NOT NULL DEFAULT '[]';
ALTER TABLE workflow.workflow_instance ADD COLUMN IF NOT EXISTS steps_snapshot JSONB;
ALTER TABLE workflow.workflow_step ADD COLUMN IF NOT EXISTS step_def_id TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow.workflow_step ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'pending';
