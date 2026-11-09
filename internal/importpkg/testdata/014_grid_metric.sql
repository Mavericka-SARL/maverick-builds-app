-- Test-schema mirror of the grids a full reload reads (production:
-- migrations 019 and 051): a grid and its metrics, a metric on at most one
-- grid. A full reload clears the grids its file writes into, no other.
CREATE TABLE IF NOT EXISTS model.grid_def (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id   UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS model.grid_metric (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    grid_id    UUID NOT NULL REFERENCES model.grid_def(id) ON DELETE CASCADE,
    metric_id  UUID NOT NULL UNIQUE REFERENCES model.metric_def(id) ON DELETE CASCADE,
    sort_order INT NOT NULL DEFAULT 0
);
