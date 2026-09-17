-- Multi-model apps need a deliberate answer to "which model do business
-- surfaces show" — resolution used to be newest-first, so CREATING a second
-- model instantly hid the first from every business console (dashboards,
-- workflows, grids all empty). The default model is what business users
-- get; developers pin other models explicitly via their revision selector.
-- Backfill: the app's OLDEST model — a no-op for single-model apps and the
-- original model for apps that later grew experiments.
ALTER TABLE core.application
    ADD COLUMN IF NOT EXISTS default_model_id UUID REFERENCES core.model(id) ON DELETE SET NULL;

UPDATE core.application a
SET default_model_id = (
    SELECT m.id FROM core.model m
    WHERE m.application_id = a.id
    ORDER BY m.created_at ASC LIMIT 1
)
WHERE a.default_model_id IS NULL;
