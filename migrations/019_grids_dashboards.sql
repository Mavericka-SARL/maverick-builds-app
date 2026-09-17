-- Aggregation rule moves to metrics (how to aggregate across dimension members)
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS agg_rule TEXT NOT NULL DEFAULT 'sum';

-- Grid definitions: a named combination of metrics + dimensions
CREATE TABLE IF NOT EXISTS model.grid_def (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id   UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS model.grid_metric (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    grid_id    UUID NOT NULL REFERENCES model.grid_def(id) ON DELETE CASCADE,
    metric_id  UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    sort_order INT NOT NULL DEFAULT 0,
    UNIQUE(grid_id, metric_id)
);

CREATE TABLE IF NOT EXISTS model.grid_dimension (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    grid_id      UUID NOT NULL REFERENCES model.grid_def(id) ON DELETE CASCADE,
    dimension_id UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    UNIQUE(grid_id, dimension_id)
);

-- Dashboard definitions: named canvas combining grids, forms, automation buttons, text
CREATE TABLE IF NOT EXISTS model.dashboard_def (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id   UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS model.dashboard_widget (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dashboard_id UUID NOT NULL REFERENCES model.dashboard_def(id) ON DELETE CASCADE,
    widget_type  TEXT NOT NULL,   -- 'grid' | 'form' | 'automation_button' | 'text'
    ref_id       TEXT,            -- ID of referenced grid/form/rule
    content      TEXT,            -- title for grid/form/button, or body for text
    sort_order   INT NOT NULL DEFAULT 0
);
