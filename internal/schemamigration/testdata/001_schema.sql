-- Minimal schema for schemamigration integration tests.
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE SCHEMA IF NOT EXISTS core;
CREATE SCHEMA IF NOT EXISTS identity;
CREATE SCHEMA IF NOT EXISTS model;
CREATE SCHEMA IF NOT EXISTS deployment;
CREATE SCHEMA IF NOT EXISTS runtime;

CREATE TABLE core.customer (
    id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL
);

CREATE TABLE core.workspace (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id UUID NOT NULL REFERENCES core.customer(id),
    name        TEXT NOT NULL
);

CREATE TABLE core.application (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES core.workspace(id),
    name         TEXT NOT NULL
);

CREATE TABLE core.model (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id),
    name           TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE core.schema_version (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id       UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    version_number INT NOT NULL,
    schema_hash    TEXT NOT NULL,
    published_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, version_number)
);

CREATE TABLE model.metric_def (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id UUID NOT NULL REFERENCES core.model(id),
    name     TEXT NOT NULL,
    formula  TEXT,
    is_input BOOLEAN NOT NULL DEFAULT false
);

-- runtime tables are referenced by generated views
CREATE TABLE runtime.fact_input (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id   UUID NOT NULL,
    scenario   TEXT NOT NULL,
    version    TEXT NOT NULL,
    dim_members JSONB NOT NULL DEFAULT '{}',
    metric_id  UUID NOT NULL,
    value      DOUBLE PRECISION NOT NULL,
    entered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE runtime.calc_result (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id      UUID NOT NULL,
    scenario      TEXT NOT NULL,
    version       TEXT NOT NULL,
    metric_id     UUID NOT NULL,
    dim_members   JSONB NOT NULL DEFAULT '{}',
    value         DOUBLE PRECISION NOT NULL,
    partition_key TEXT NOT NULL,
    calc_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE deployment.schema_migration (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id       UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    revision_id    UUID REFERENCES core.schema_version(id) ON DELETE SET NULL,
    version_number INT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'applied', 'rolled_back', 'failed')),
    files          JSONB NOT NULL DEFAULT '[]',
    error          TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at     TIMESTAMPTZ,
    rolled_back_at TIMESTAMPTZ,
    UNIQUE (model_id, version_number)
);
