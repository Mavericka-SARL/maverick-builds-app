-- Schema: runtime
-- Fact inputs, calculation results, partition state

CREATE SCHEMA IF NOT EXISTS runtime;

-- Raw input values written by users (writeback)
-- Partitioned by model_id + month for performance at scale
CREATE TABLE runtime.fact_input (
    id           UUID NOT NULL DEFAULT gen_random_uuid(),
    model_id     UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    scenario     TEXT NOT NULL,
    version      TEXT NOT NULL,
    dim_members  JSONB NOT NULL,   -- {dimension_id: member_code, ...}
    metric_id    UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    value        NUMERIC NOT NULL,
    entered_by   UUID NOT NULL REFERENCES identity.user(id),
    entered_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, entered_at)
) PARTITION BY RANGE (entered_at);

-- Create initial monthly partition covering current month
CREATE TABLE runtime.fact_input_default PARTITION OF runtime.fact_input DEFAULT;

CREATE INDEX ON runtime.fact_input (model_id, scenario, version, metric_id);
CREATE INDEX ON runtime.fact_input USING GIN (dim_members);

-- Calculated results produced by the calculation engine
CREATE TABLE runtime.calc_result (
    id            UUID NOT NULL DEFAULT gen_random_uuid(),
    model_id      UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    scenario      TEXT NOT NULL,
    version       TEXT NOT NULL,
    dim_members   JSONB NOT NULL,
    metric_id     UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    value         NUMERIC,
    partition_key TEXT NOT NULL,   -- composite key for the metric partition
    calc_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, calc_at)
) PARTITION BY RANGE (calc_at);

CREATE TABLE runtime.calc_result_default PARTITION OF runtime.calc_result DEFAULT;

CREATE INDEX ON runtime.calc_result (model_id, scenario, version, metric_id);
CREATE INDEX ON runtime.calc_result (partition_key);

-- Tracks the clean/dirty/calculating/error state per metric partition
CREATE TYPE runtime.partition_status AS ENUM ('clean', 'dirty', 'calculating', 'error');

CREATE TABLE runtime.metric_partition_state (
    partition_key TEXT PRIMARY KEY,
    model_id      UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    metric_id     UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    scenario      TEXT NOT NULL,
    version       TEXT NOT NULL,
    time_partition TEXT NOT NULL,
    status        runtime.partition_status NOT NULL DEFAULT 'dirty',
    last_calc_at  TIMESTAMPTZ,
    error         TEXT,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON runtime.metric_partition_state (model_id, status);
CREATE INDEX ON runtime.metric_partition_state (status, updated_at);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON runtime.metric_partition_state
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
