-- Integrations become first-class saved objects: a draft status (the Import
-- Wizard can save work-in-progress before it is runnable), free-form tags,
-- and (already present) a name that PATCH can change.
ALTER TABLE model.integration_def ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active';
ALTER TABLE model.integration_def ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
