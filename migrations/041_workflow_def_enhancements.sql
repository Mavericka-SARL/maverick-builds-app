-- Enhance workflow_def for the Developer Workflows builder

ALTER TABLE workflow.workflow_def
  ADD COLUMN IF NOT EXISTS description   TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS status        TEXT NOT NULL DEFAULT 'draft',
  ADD COLUMN IF NOT EXISTS created_by    UUID REFERENCES identity.user(id),
  ADD COLUMN IF NOT EXISTS updated_by    UUID REFERENCES identity.user(id),
  ADD COLUMN IF NOT EXISTS published_at  TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS archived_at   TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS context_schema JSONB NOT NULL DEFAULT '[]';

-- Add workflow_def_id to automation_rule so rules can reference a stable id
ALTER TABLE workflow.automation_rule
  ADD COLUMN IF NOT EXISTS workflow_def_id UUID REFERENCES workflow.workflow_def(id) ON DELETE SET NULL;
