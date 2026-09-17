-- Minimal schema for crudapp integration tests.
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE SCHEMA IF NOT EXISTS core;
CREATE SCHEMA IF NOT EXISTS identity;
CREATE SCHEMA IF NOT EXISTS model;
CREATE SCHEMA IF NOT EXISTS runtime;

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

CREATE TABLE model.form_def (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id   UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    label      TEXT NOT NULL,
    fields     JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(model_id, name)
);

CREATE TYPE runtime.record_status AS ENUM ('draft', 'submitted', 'approved', 'rejected');

CREATE TABLE runtime.form_record (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    form_id    UUID NOT NULL REFERENCES model.form_def(id) ON DELETE CASCADE,
    data       JSONB NOT NULL DEFAULT '{}',
    status     runtime.record_status NOT NULL DEFAULT 'draft',
    created_by UUID NOT NULL REFERENCES identity.user(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
