-- Extends the minimal query-package test schema (001_schema.sql) with what
-- ChartResolver needs: a dimension hierarchy, grid definitions, and access
-- rules — none of which store_test.go's narrower Store-level tests touch.
-- Deliberately skips FKs to tables this package's tests never populate
-- (core.model, model.form_def), matching 001_schema.sql's existing
-- convention of a minimal, not fully-normalized, standalone test schema.

ALTER TABLE model.metric_def ADD COLUMN revision_id UUID;
ALTER TABLE model.metric_def ADD COLUMN agg_rule TEXT NOT NULL DEFAULT 'sum';

ALTER TABLE runtime.fact_input ADD COLUMN source_ref UUID;

-- Stand-in for model.form_metric_mapping: getInputValue LEFT JOINs this to
-- exclude form-sourced facts superseded by a 'replace'/'last' aggregation.
-- Only the two columns that query actually references are needed.
CREATE TABLE model.form_metric_mapping (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregation TEXT NOT NULL DEFAULT 'sum'
);

CREATE TABLE model.dimension_def (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id            UUID NOT NULL,
    name                TEXT NOT NULL,
    revision_id         UUID,
    parent_dimension_id UUID REFERENCES model.dimension_def(id) ON DELETE SET NULL
);

CREATE TABLE model.dimension_member (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dimension_id     UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    code             TEXT NOT NULL,
    label            TEXT NOT NULL,
    sort_order       INT NOT NULL DEFAULT 0,
    parent_member_id UUID REFERENCES model.dimension_member(id) ON DELETE SET NULL,
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
    PRIMARY KEY (grid_id, metric_id)
);

CREATE TABLE identity.user_access_rule (
    user_id   UUID NOT NULL,
    rule_type TEXT NOT NULL,
    ref_id    TEXT NOT NULL,
    access    TEXT NOT NULL,
    PRIMARY KEY (user_id, rule_type, ref_id)
);
