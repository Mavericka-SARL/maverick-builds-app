ALTER TABLE core.model
  ADD COLUMN IF NOT EXISTS active_scenario TEXT,
  ADD COLUMN IF NOT EXISTS active_version  TEXT;
