-- Scope grid definitions to a revision so each revision has its own grid configuration.
ALTER TABLE model.grid_def
  ADD COLUMN IF NOT EXISTS revision_id UUID REFERENCES model.scenario(id) ON DELETE CASCADE;

-- Backfill existing grids to the model's active revision
UPDATE model.grid_def g
SET revision_id = m.active_revision_id
FROM core.model m
WHERE g.model_id = m.id
  AND g.revision_id IS NULL
  AND m.active_revision_id IS NOT NULL;

-- Remaining (no active revision): assign to most recent scenario
UPDATE model.grid_def g
SET revision_id = (
  SELECT id FROM model.scenario
  WHERE model_id = g.model_id
  ORDER BY created_at DESC LIMIT 1
)
WHERE g.revision_id IS NULL;

CREATE INDEX IF NOT EXISTS grid_def_revision_idx ON model.grid_def (revision_id);
