-- Schema: workflow
-- Workflow definitions, instances, steps

CREATE SCHEMA IF NOT EXISTS workflow;

CREATE TYPE workflow.step_type AS ENUM ('task', 'approval', 'notification', 'condition');
CREATE TYPE workflow.workflow_status AS ENUM ('running', 'completed', 'cancelled', 'failed');
CREATE TYPE workflow.step_status AS ENUM ('pending', 'in_progress', 'completed', 'rejected', 'skipped');

CREATE TABLE workflow.workflow_def (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    trigger_event  TEXT NOT NULL,
    steps          JSONB NOT NULL DEFAULT '[]',   -- serialized WorkflowStepDef array
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (application_id, name)
);

CREATE INDEX ON workflow.workflow_def (application_id);

CREATE TABLE workflow.workflow_instance (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_def_id UUID NOT NULL REFERENCES workflow.workflow_def(id),
    status          workflow.workflow_status NOT NULL DEFAULT 'running',
    started_by      UUID NOT NULL REFERENCES identity.user(id),
    context         JSONB NOT NULL DEFAULT '{}',
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

CREATE INDEX ON workflow.workflow_instance (workflow_def_id, status);
CREATE INDEX ON workflow.workflow_instance (started_by);

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

CREATE INDEX ON workflow.workflow_step (instance_id);
CREATE INDEX ON workflow.workflow_step (assignee_user_id, status);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON workflow.workflow_def
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
