-- Minimal schema for workflow package integration tests.
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE SCHEMA IF NOT EXISTS core;
CREATE SCHEMA IF NOT EXISTS identity;
CREATE SCHEMA IF NOT EXISTS workflow;

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

CREATE TYPE workflow.step_type AS ENUM ('task', 'approval', 'notification', 'condition');
CREATE TYPE workflow.workflow_status AS ENUM ('running', 'completed', 'cancelled', 'failed');
CREATE TYPE workflow.step_status AS ENUM ('pending', 'in_progress', 'completed', 'rejected', 'skipped');

CREATE TABLE workflow.workflow_def (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    trigger_event  TEXT NOT NULL,
    steps          JSONB NOT NULL DEFAULT '[]',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (application_id, name)
);

CREATE TABLE workflow.workflow_instance (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_def_id UUID NOT NULL REFERENCES workflow.workflow_def(id),
    status          workflow.workflow_status NOT NULL DEFAULT 'running',
    started_by      UUID NOT NULL REFERENCES identity.user(id),
    context         JSONB NOT NULL DEFAULT '{}',
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

CREATE TABLE workflow.workflow_step (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    instance_id      UUID NOT NULL REFERENCES workflow.workflow_instance(id) ON DELETE CASCADE,
    step_def_id      TEXT NOT NULL,
    status           workflow.step_status NOT NULL DEFAULT 'pending',
    assignee_user_id UUID REFERENCES identity.user(id),
    decision         TEXT,
    comment          TEXT,
    due_at           TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
