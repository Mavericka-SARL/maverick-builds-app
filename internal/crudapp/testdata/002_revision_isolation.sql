-- Mirrors migration 056: forms are revision-scoped. No FK here — the
-- minimal test schema has no model.revision table; production enforces it.

ALTER TABLE model.form_def
    ADD COLUMN IF NOT EXISTS revision_id UUID;

ALTER TABLE model.form_def DROP CONSTRAINT IF EXISTS form_def_model_id_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS form_def_rev_name_uq
    ON model.form_def (model_id, revision_id, name) NULLS NOT DISTINCT;
