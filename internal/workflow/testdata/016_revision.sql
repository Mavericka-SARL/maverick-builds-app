-- Reduced schema: the revisions of a model, so the scheduler can tell a
-- schedule rule of the live revision from one of another (scheduler.go,
-- activeRevisionRuleSQL).
CREATE TABLE IF NOT EXISTS model.revision (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name     TEXT NOT NULL DEFAULT ''
);
