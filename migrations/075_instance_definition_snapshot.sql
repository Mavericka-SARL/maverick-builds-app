-- A running instance keeps the definition it started with. Until now the
-- engine re-read workflow_def.steps on every step completion, so editing a
-- published workflow silently changed in-flight approvals (routes,
-- assignees, conditions). The snapshot is taken at start; instances from
-- before this migration have NULL and keep reading the live definition.
ALTER TABLE workflow.workflow_instance
    ADD COLUMN IF NOT EXISTS steps_snapshot          JSONB,
    ADD COLUMN IF NOT EXISTS context_schema_snapshot JSONB;
