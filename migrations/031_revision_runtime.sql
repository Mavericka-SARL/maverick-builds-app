-- Revision-scoped runtime data.
-- 1. Set active_revision_id on all models that have revisions but no active set.
-- 2. Add revision_id to runtime tables for proper revision scoping.
-- 3. Backfill existing runtime rows to the FY2026 Budget revision.

-- Activate each model's most-recently-created revision if not already set.
UPDATE core.model m
SET active_revision_id = (
    SELECT s.id FROM model.scenario s WHERE s.model_id = m.id ORDER BY s.created_at DESC LIMIT 1
),
    active_scenario = (
    SELECT s.name FROM model.scenario s WHERE s.model_id = m.id ORDER BY s.created_at DESC LIMIT 1
),
    active_version = 'draft'
WHERE m.active_revision_id IS NULL
  AND EXISTS (SELECT 1 FROM model.scenario s WHERE s.model_id = m.id);

-- Add revision_id to runtime tables (no FK — partitioned tables have FK limitations).
ALTER TABLE runtime.fact_input             ADD COLUMN IF NOT EXISTS revision_id UUID;
ALTER TABLE runtime.calc_result            ADD COLUMN IF NOT EXISTS revision_id UUID;
ALTER TABLE runtime.metric_partition_state ADD COLUMN IF NOT EXISTS revision_id UUID;

-- Backfill existing runtime rows: link to the revision whose name matches the scenario column.
UPDATE runtime.fact_input fi
SET revision_id = s.id
FROM model.scenario s
WHERE fi.revision_id IS NULL
  AND s.name = fi.scenario;

UPDATE runtime.calc_result cr
SET revision_id = s.id
FROM model.scenario s
WHERE cr.revision_id IS NULL
  AND s.name = cr.scenario;

-- Fast revision-scoped queries.
CREATE INDEX IF NOT EXISTS fact_input_revision_idx    ON runtime.fact_input             (model_id, revision_id, metric_id);
CREATE INDEX IF NOT EXISTS calc_result_revision_idx   ON runtime.calc_result            (model_id, revision_id, metric_id);
CREATE INDEX IF NOT EXISTS mps_revision_idx           ON runtime.metric_partition_state (revision_id);
