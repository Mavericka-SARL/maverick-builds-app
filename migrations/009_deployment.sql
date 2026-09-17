-- Schema: deployment
-- App package builds and export artifacts

CREATE SCHEMA IF NOT EXISTS deployment;

CREATE TYPE deployment.deployment_status AS ENUM (
    'building', 'ready', 'deployed', 'failed', 'rolled_back'
);

CREATE TABLE deployment.deployment_job (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    revision_id  UUID REFERENCES core.revision(id) ON DELETE SET NULL,
    status       deployment.deployment_status NOT NULL DEFAULT 'building',
    standalone   BOOLEAN NOT NULL DEFAULT false,
    artifact_url TEXT,
    error        TEXT,
    created_by   UUID NOT NULL REFERENCES identity.user(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

CREATE INDEX ON deployment.deployment_job (application_id, status);

-- Migration run log: tracks which SQL migrations have been applied per model
CREATE TABLE deployment.migration_run (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id        UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    revision_id     UUID REFERENCES core.revision(id),
    version_number  INT NOT NULL,
    filename        TEXT NOT NULL,
    checksum        TEXT NOT NULL,
    applied_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    rolled_back_at  TIMESTAMPTZ,
    UNIQUE (model_id, version_number)
);

CREATE INDEX ON deployment.migration_run (model_id);
