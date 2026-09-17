-- Tables internal/writeguard.CheckWrite reads (system_managed revision check,
-- hidden/read-only dimension_member access rules, workflow-lock scoping).
-- Mirrors production migrations 003/007/018/027 at the columns writeguard
-- actually queries. No FKs to identity.user beyond what's already declared —
-- production enforces the rest.

CREATE TABLE model.revision (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id       UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name           TEXT NOT NULL DEFAULT '',
    system_managed BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE model.dimension_def (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name     TEXT NOT NULL
);

CREATE TABLE model.dimension_member (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dimension_id     UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    code             TEXT NOT NULL,
    parent_member_id UUID REFERENCES model.dimension_member(id) ON DELETE SET NULL,
    UNIQUE (dimension_id, code)
);

CREATE TABLE identity.user_access_rule (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id   UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    rule_type TEXT NOT NULL,
    ref_id    TEXT NOT NULL,
    access    TEXT NOT NULL
);

CREATE SCHEMA IF NOT EXISTS workflow;

CREATE TABLE workflow.workflow_def (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    context_schema JSONB NOT NULL DEFAULT '[]'
);

CREATE TABLE workflow.workflow_instance (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_def_id UUID NOT NULL REFERENCES workflow.workflow_def(id) ON DELETE CASCADE,
    context         JSONB NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'running',
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE workflow.workflow_step (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    instance_id  UUID NOT NULL REFERENCES workflow.workflow_instance(id) ON DELETE CASCADE,
    decision     TEXT,
    completed_at TIMESTAMPTZ
);
