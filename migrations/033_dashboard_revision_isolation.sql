-- Scope dashboards to a revision so each revision has its own set of dashboards.
-- Widgets inherit isolation implicitly via dashboard_id FK.

ALTER TABLE model.dashboard_def
  ADD COLUMN IF NOT EXISTS revision_id UUID REFERENCES model.scenario(id) ON DELETE CASCADE;

-- Backfill existing dashboards to the active revision of their model
UPDATE model.dashboard_def d
SET revision_id = m.active_revision_id
FROM core.model m
WHERE d.model_id = m.id
  AND d.revision_id IS NULL
  AND m.active_revision_id IS NOT NULL;

-- Any remaining (model has no active revision): assign to most recent scenario
UPDATE model.dashboard_def d
SET revision_id = (
  SELECT id FROM model.scenario
  WHERE model_id = d.model_id
  ORDER BY created_at DESC
  LIMIT 1
)
WHERE d.revision_id IS NULL;

CREATE INDEX IF NOT EXISTS dashboard_def_revision_idx ON model.dashboard_def (revision_id);
