-- Schema: audit
-- Append-only event log; partitioned by month

CREATE SCHEMA IF NOT EXISTS audit;

CREATE TYPE audit.event_category AS ENUM (
    'auth',
    'data_change',
    'model_change',
    'policy_change',
    'admin',
    'ai_assistant'
);

CREATE TABLE audit.audit_event (
    id            UUID NOT NULL DEFAULT gen_random_uuid(),
    category      audit.event_category NOT NULL,
    event_type    TEXT NOT NULL,
    actor_user_id UUID REFERENCES identity.user(id) ON DELETE SET NULL,
    actor_role    TEXT,
    workspace_id  UUID REFERENCES core.workspace(id) ON DELETE SET NULL,
    resource_type TEXT,
    resource_id   TEXT,
    metadata      JSONB NOT NULL DEFAULT '{}',
    before_state  JSONB,
    after_state   JSONB,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

-- Default catch-all partition; monthly partitions created by audit service
CREATE TABLE audit.audit_event_default PARTITION OF audit.audit_event DEFAULT;

CREATE INDEX ON audit.audit_event (actor_user_id, occurred_at);
CREATE INDEX ON audit.audit_event (resource_type, resource_id, occurred_at);
CREATE INDEX ON audit.audit_event (category, occurred_at);
CREATE INDEX ON audit.audit_event (workspace_id, occurred_at);

-- Tracks partition existence so the audit service can create new monthly ones
CREATE TABLE audit.partition_registry (
    partition_name TEXT PRIMARY KEY,
    range_start    TIMESTAMPTZ NOT NULL,
    range_end      TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
