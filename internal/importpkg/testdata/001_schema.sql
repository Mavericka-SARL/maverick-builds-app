-- Minimal schema for importpkg integration tests.
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE SCHEMA IF NOT EXISTS core;
CREATE SCHEMA IF NOT EXISTS identity;
CREATE SCHEMA IF NOT EXISTS model;
CREATE SCHEMA IF NOT EXISTS runtime;
CREATE SCHEMA IF NOT EXISTS import;

CREATE TABLE identity.user (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email      TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

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

CREATE TABLE model.metric_def (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id UUID NOT NULL REFERENCES core.model(id),
    name     TEXT NOT NULL,
    is_input BOOLEAN NOT NULL DEFAULT false
);

-- runtime.fact_input: destination for committed import rows
CREATE TABLE runtime.fact_input (
    id          UUID NOT NULL DEFAULT gen_random_uuid(),
    model_id    UUID NOT NULL REFERENCES core.model(id),
    revision_id UUID,
    dim_members JSONB NOT NULL DEFAULT '{}',
    metric_id   UUID NOT NULL REFERENCES model.metric_def(id),
    value       NUMERIC NOT NULL,
    entered_by  UUID NOT NULL REFERENCES identity.user(id),
    entered_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, entered_at)
) PARTITION BY RANGE (entered_at);

CREATE TABLE runtime.fact_input_default PARTITION OF runtime.fact_input DEFAULT;

CREATE TYPE import.import_status AS ENUM (
    'validating', 'staged', 'committed', 'failed', 'aborted'
);

CREATE TABLE import.import_job (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id     UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    revision_id  UUID,
    scenario     TEXT,
    version      TEXT,
    status       import.import_status NOT NULL DEFAULT 'validating',
    mappings     JSONB NOT NULL DEFAULT '[]',
    file_url     TEXT NOT NULL,
    total_rows   INT NOT NULL DEFAULT 0,
    valid_rows   INT NOT NULL DEFAULT 0,
    error_rows   INT NOT NULL DEFAULT 0,
    created_by   UUID NOT NULL REFERENCES identity.user(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

CREATE TABLE import.import_error (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id      UUID NOT NULL REFERENCES import.import_job(id) ON DELETE CASCADE,
    row_number  INT NOT NULL,
    column_name TEXT,
    error_code  TEXT NOT NULL,
    message     TEXT NOT NULL,
    raw_value   TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE import.import_staging (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id      UUID NOT NULL REFERENCES import.import_job(id) ON DELETE CASCADE,
    metric_id   UUID NOT NULL,
    dim_members JSONB NOT NULL DEFAULT '{}',
    value       NUMERIC NOT NULL,
    row_number  INT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
