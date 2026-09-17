-- Minimal schema for model package integration tests.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE SCHEMA IF NOT EXISTS core;
CREATE SCHEMA IF NOT EXISTS model;

CREATE TYPE core.storage_type AS ENUM ('oltp', 'columnar', 'hybrid');

-- core.model is the target for PublishRevision
CREATE TABLE core.model (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL,
    name           TEXT NOT NULL,
    storage_type   TEXT NOT NULL DEFAULT 'oltp',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE core.schema_version (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id       UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    version_number INT  NOT NULL,
    schema_hash    TEXT NOT NULL,
    published_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, version_number)
);

CREATE TABLE model.dimension_def (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id   UUID NOT NULL,
    name       TEXT NOT NULL,
    properties JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, name)
);

CREATE TABLE model.dimension_member (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dimension_id UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    code         TEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT '',
    parent_id    UUID REFERENCES model.dimension_member(id),
    sort_order   INT NOT NULL DEFAULT 0,
    properties   JSONB NOT NULL DEFAULT '{}',
    UNIQUE (dimension_id, code)
);

CREATE TABLE model.metric_def (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id     UUID NOT NULL,
    name         TEXT NOT NULL,
    formula      TEXT,
    storage_type core.storage_type NOT NULL DEFAULT 'oltp',
    is_input     BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, name)
);

CREATE TABLE model.calc_dependency (
    metric_id            UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    depends_on_metric_id UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    PRIMARY KEY (metric_id, depends_on_metric_id)
);

CREATE TABLE model.hierarchy (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id     UUID NOT NULL,
    name         TEXT NOT NULL,
    dimension_id UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    level_names  TEXT[] NOT NULL DEFAULT '{}',
    UNIQUE (model_id, name)
);

CREATE TABLE model.scenario (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id    UUID NOT NULL,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    UNIQUE (model_id, name)
);

CREATE TABLE model.version (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id  UUID NOT NULL,
    name      TEXT NOT NULL,
    is_locked BOOLEAN NOT NULL DEFAULT false,
    UNIQUE (model_id, name)
);
