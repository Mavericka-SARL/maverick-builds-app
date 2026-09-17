-- Mirrors migration 056: forms and integrations carry a revision_id. No FK
-- here — the minimal test schema has no model.revision table; production
-- enforces it.

ALTER TABLE model.form_def
    ADD COLUMN IF NOT EXISTS revision_id UUID;

ALTER TABLE model.integration_def
    ADD COLUMN IF NOT EXISTS revision_id UUID;
