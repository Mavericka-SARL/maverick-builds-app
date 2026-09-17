-- Schema: import
-- Bulk import jobs: validate → stage → commit pipeline

CREATE SCHEMA IF NOT EXISTS import;

CREATE TYPE import.import_status AS ENUM (
    'validating', 'staged', 'committed', 'failed', 'aborted'
);

CREATE TABLE import.import_job (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id     UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    scenario     TEXT NOT NULL,
    version      TEXT NOT NULL,
    status       import.import_status NOT NULL DEFAULT 'validating',
    mappings     JSONB NOT NULL DEFAULT '[]',   -- [{source_column, target_field}]
    file_url     TEXT NOT NULL,
    total_rows   INT NOT NULL DEFAULT 0,
    valid_rows   INT NOT NULL DEFAULT 0,
    error_rows   INT NOT NULL DEFAULT 0,
    created_by   UUID NOT NULL REFERENCES identity.user(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

CREATE INDEX ON import.import_job (model_id, status);

-- Individual import errors with row-level detail
CREATE TABLE import.import_error (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id     UUID NOT NULL REFERENCES import.import_job(id) ON DELETE CASCADE,
    row_number INT NOT NULL,
    column_name TEXT,
    error_code TEXT NOT NULL,
    message    TEXT NOT NULL,
    raw_value  TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON import.import_error (job_id);
