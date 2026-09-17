-- Revision isolation: metric/dimension definitions are scoped per revision (model.scenario).
-- Developer works in a specific revision; business users see only the active revision's data.

-- Drop old unique constraints that don't account for revision_id
ALTER TABLE model.metric_def    DROP CONSTRAINT IF EXISTS metric_def_model_id_name_key;
ALTER TABLE model.dimension_def DROP CONSTRAINT IF EXISTS dimension_def_model_id_name_key;

-- Revision scope for metric definitions
ALTER TABLE model.metric_def
  ADD COLUMN IF NOT EXISTS revision_id UUID REFERENCES model.scenario(id) ON DELETE CASCADE;

-- Partial unique indexes: same metric name is allowed in different revisions, but not within the same revision
CREATE UNIQUE INDEX IF NOT EXISTS metric_def_null_rev_uq
  ON model.metric_def (model_id, name) WHERE revision_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS metric_def_with_rev_uq
  ON model.metric_def (model_id, revision_id, name) WHERE revision_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS metric_def_revision_idx ON model.metric_def (revision_id);

-- Revision scope for dimension definitions
ALTER TABLE model.dimension_def
  ADD COLUMN IF NOT EXISTS revision_id UUID REFERENCES model.scenario(id) ON DELETE CASCADE;

CREATE UNIQUE INDEX IF NOT EXISTS dimension_def_null_rev_uq
  ON model.dimension_def (model_id, name) WHERE revision_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS dimension_def_with_rev_uq
  ON model.dimension_def (model_id, revision_id, name) WHERE revision_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS dimension_def_revision_idx ON model.dimension_def (revision_id);

-- Denormalized active revision ID on model for faster joins (set when admin activates a revision)
ALTER TABLE core.model
  ADD COLUMN IF NOT EXISTS active_revision_id UUID REFERENCES model.scenario(id) ON DELETE SET NULL;

-- Audit events tagged with the revision they occurred in
ALTER TABLE audit.audit_event ADD COLUMN IF NOT EXISTS revision_id UUID;
