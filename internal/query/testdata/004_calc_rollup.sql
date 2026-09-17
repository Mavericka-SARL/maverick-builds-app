-- Extends the test schema with what chart.go's rollup wiring needs beyond
-- 002_charts.sql: metric format fields (loadGridMetrics selects them
-- unconditionally now) and calc_dependency (calc-metric dependency
-- resolution, exercised by the new calc-metric rollup test).

ALTER TABLE model.metric_def ADD COLUMN format          TEXT     NOT NULL DEFAULT 'number';
ALTER TABLE model.metric_def ADD COLUMN format_decimals SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE model.metric_def ADD COLUMN format_currency TEXT     NOT NULL DEFAULT '$';

CREATE TABLE model.calc_dependency (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    metric_id            UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    depends_on_metric_id UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    UNIQUE (metric_id, depends_on_metric_id)
);

ALTER TABLE model.dimension_def ADD COLUMN source_dimension_id UUID REFERENCES model.dimension_def(id) ON DELETE SET NULL;
ALTER TABLE model.dimension_def ADD COLUMN source_property     TEXT;
ALTER TABLE model.dimension_member ADD COLUMN properties JSONB;
