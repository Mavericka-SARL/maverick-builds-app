-- Extends the test schema with what per-intersection persistence needs
-- beyond 001_schema.sql: revision-scoping on metric_def/dimension_def
-- (LoadModelMetrics/LoadDependencyGraph/LoadDimIDToName are now strictly
-- revision-scoped), cross-dimension linking, and the grid_def/grid_dimension/
-- grid_metric config LoadAllDimensions/LoadMetricDimensionIDs read to
-- enumerate a metric's own declared leaf-level intersections.

ALTER TABLE model.metric_def ADD COLUMN revision_id UUID;
ALTER TABLE model.dimension_def ADD COLUMN revision_id UUID;
ALTER TABLE model.dimension_def ADD COLUMN parent_dimension_id UUID REFERENCES model.dimension_def(id) ON DELETE SET NULL;
ALTER TABLE model.dimension_def ADD COLUMN source_dimension_id UUID REFERENCES model.dimension_def(id) ON DELETE SET NULL;
ALTER TABLE model.dimension_def ADD COLUMN source_property TEXT;

CREATE TABLE model.dimension_member (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dimension_id     UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    code             TEXT NOT NULL,
    label            TEXT NOT NULL,
    sort_order       INT NOT NULL DEFAULT 0,
    parent_member_id UUID REFERENCES model.dimension_member(id) ON DELETE SET NULL,
    properties       JSONB,
    UNIQUE (dimension_id, code)
);

CREATE TABLE model.grid_def (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id UUID NOT NULL,
    name     TEXT NOT NULL
);

CREATE TABLE model.grid_dimension (
    grid_id      UUID NOT NULL REFERENCES model.grid_def(id) ON DELETE CASCADE,
    dimension_id UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    PRIMARY KEY (grid_id, dimension_id)
);

CREATE TABLE model.grid_metric (
    grid_id    UUID NOT NULL REFERENCES model.grid_def(id) ON DELETE CASCADE,
    metric_id  UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    sort_order INT NOT NULL DEFAULT 0,
    PRIMARY KEY (grid_id, metric_id),
    UNIQUE (metric_id)
);
