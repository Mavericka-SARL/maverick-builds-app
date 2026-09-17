-- Minimal schema for query package integration tests.
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE SCHEMA IF NOT EXISTS identity;
CREATE SCHEMA IF NOT EXISTS model;
CREATE SCHEMA IF NOT EXISTS runtime;

CREATE TABLE identity.user (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email      TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE model.metric_def (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id UUID NOT NULL,
    name     TEXT NOT NULL,
    formula  TEXT,
    is_input BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE runtime.fact_input (
    id          UUID NOT NULL DEFAULT gen_random_uuid(),
    model_id    UUID NOT NULL,
    revision_id UUID,
    dim_members JSONB NOT NULL DEFAULT '{}',
    metric_id   UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    value       NUMERIC NOT NULL,
    entered_by  UUID NOT NULL,
    entered_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, entered_at)
) PARTITION BY RANGE (entered_at);

CREATE TABLE runtime.fact_input_default PARTITION OF runtime.fact_input DEFAULT;
CREATE INDEX ON runtime.fact_input (model_id, revision_id, metric_id);

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
