-- Execution Application Mode: automation rules and execution log

CREATE TYPE workflow.trigger_type AS ENUM ('manual', 'form_submit', 'api');

CREATE TABLE workflow.automation_rule (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    description    TEXT,
    trigger_type   workflow.trigger_type NOT NULL DEFAULT 'manual',
    workflow_name  TEXT NOT NULL,
    enabled        BOOLEAN NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(application_id, name)
);

CREATE TYPE workflow.execution_status AS ENUM ('running', 'completed', 'failed', 'cancelled');

CREATE TABLE workflow.execution (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id         UUID REFERENCES workflow.automation_rule(id) ON DELETE SET NULL,
    application_id  UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    status          workflow.execution_status NOT NULL DEFAULT 'running',
    trigger_payload JSONB NOT NULL DEFAULT '{}',
    instance_id     UUID REFERENCES workflow.workflow_instance(id) ON DELETE SET NULL,
    error           TEXT,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

CREATE INDEX ON workflow.execution (application_id, started_at DESC);
CREATE INDEX ON workflow.execution (rule_id);
