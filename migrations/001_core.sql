-- Schema: core
-- Product hierarchy: customer > workspace > application > model > revision

CREATE SCHEMA IF NOT EXISTS core;

CREATE TABLE core.customer (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    plan        TEXT NOT NULL DEFAULT 'starter',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE core.workspace (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id UUID NOT NULL REFERENCES core.customer(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    settings    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON core.workspace (customer_id);

CREATE TYPE core.application_mode AS ENUM ('planning', 'crud', 'execution');
CREATE TYPE core.application_status AS ENUM ('draft', 'published', 'archived');

CREATE TABLE core.application (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES core.workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    mode         core.application_mode NOT NULL,
    status       core.application_status NOT NULL DEFAULT 'draft',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON core.application (workspace_id);
CREATE INDEX ON core.application (status);

CREATE TYPE core.storage_type AS ENUM ('oltp', 'columnar', 'hybrid');

CREATE TABLE core.model (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    storage_type   core.storage_type NOT NULL DEFAULT 'oltp',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON core.model (application_id);

CREATE TABLE core.revision (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id       UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    version_number INT  NOT NULL,
    schema_hash    TEXT NOT NULL,
    published_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, version_number)
);

CREATE INDEX ON core.revision (model_id);

-- Updated_at trigger function shared across schemas
CREATE OR REPLACE FUNCTION core.set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON core.customer
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON core.workspace
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON core.application
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON core.model
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
