-- Minimal schema for aiassistant integration tests.
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE SCHEMA IF NOT EXISTS core;
CREATE SCHEMA IF NOT EXISTS identity;
CREATE SCHEMA IF NOT EXISTS ai_assistant;

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

CREATE TABLE ai_assistant.session (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    user_id        UUID NOT NULL REFERENCES identity.user(id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TYPE ai_assistant.action_type AS ENUM (
    'add_metric', 'modify_formula', 'add_dimension',
    'modify_policy', 'add_workflow', 'generate_migration'
);

CREATE TABLE ai_assistant.action (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id         UUID NOT NULL REFERENCES ai_assistant.session(id) ON DELETE CASCADE,
    action_type        ai_assistant.action_type NOT NULL,
    prompt             TEXT NOT NULL,
    diffs              JSONB NOT NULL DEFAULT '[]',
    impact             JSONB NOT NULL DEFAULT '{}',
    applied            BOOLEAN NOT NULL DEFAULT false,
    rollback_action_id UUID REFERENCES ai_assistant.action(id),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
