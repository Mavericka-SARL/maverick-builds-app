-- Minimal schema for calculation package integration tests.
-- No foreign keys to core/identity to keep the fixture self-contained.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE SCHEMA IF NOT EXISTS model;
CREATE SCHEMA IF NOT EXISTS runtime;

CREATE TABLE model.metric_def (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id UUID NOT NULL,
    name     TEXT NOT NULL,
    formula  TEXT,
    is_input BOOLEAN NOT NULL DEFAULT false,
    agg_rule TEXT NOT NULL DEFAULT 'sum'
);

CREATE TABLE model.dimension_def (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id UUID NOT NULL,
    name     TEXT NOT NULL
);

CREATE TABLE model.calc_dependency (
    metric_id            UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    depends_on_metric_id UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    PRIMARY KEY (metric_id, depends_on_metric_id)
);

CREATE TABLE runtime.fact_input (
    id          UUID NOT NULL DEFAULT gen_random_uuid(),
    model_id    UUID NOT NULL,
    revision_id UUID,
    dim_members JSONB NOT NULL DEFAULT '{}',
    metric_id   UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    value       NUMERIC NOT NULL,
    entered_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, entered_at)
) PARTITION BY RANGE (entered_at);

CREATE TABLE runtime.fact_input_default PARTITION OF runtime.fact_input DEFAULT;
CREATE INDEX ON runtime.fact_input (model_id, revision_id, metric_id);
CREATE INDEX ON runtime.fact_input USING GIN (dim_members);

CREATE TABLE runtime.calc_result (
    id            UUID NOT NULL DEFAULT gen_random_uuid(),
    model_id      UUID NOT NULL,
    revision_id   UUID,
    dim_members   JSONB NOT NULL DEFAULT '{}',
    metric_id     UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    value         NUMERIC,
    partition_key TEXT NOT NULL,
    calc_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, calc_at)
) PARTITION BY RANGE (calc_at);

CREATE TABLE runtime.calc_result_default PARTITION OF runtime.calc_result DEFAULT;
CREATE INDEX ON runtime.calc_result (model_id, revision_id, metric_id);

CREATE TYPE runtime.partition_status AS ENUM ('clean', 'dirty', 'calculating', 'error');

CREATE TABLE runtime.metric_partition_state (
    partition_key  TEXT PRIMARY KEY,
    model_id       UUID NOT NULL,
    metric_id      UUID NOT NULL,
    revision_id    UUID NOT NULL,
    time_partition TEXT NOT NULL,
    status         runtime.partition_status NOT NULL DEFAULT 'dirty',
    last_calc_at   TIMESTAMPTZ,
    error          TEXT,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
