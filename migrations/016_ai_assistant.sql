-- Schema: ai_assistant
-- Sessions and actions for the developer-scoped AI assistant.

CREATE SCHEMA IF NOT EXISTS ai_assistant;

CREATE TABLE ai_assistant.session (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    user_id        UUID NOT NULL REFERENCES identity.user(id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON ai_assistant.session (application_id, user_id);

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

CREATE INDEX ON ai_assistant.action (session_id);

-- Role and grants for ai_assistant (schema created here, so grants belong here)
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'role_ai_assistant') THEN
    CREATE ROLE role_ai_assistant NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'svc_ai_assistant') THEN
    CREATE USER svc_ai_assistant PASSWORD 'svc_ai_assistant_dev';
  END IF;
END
$$;

GRANT USAGE ON SCHEMA ai_assistant TO role_ai_assistant;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ai_assistant TO role_ai_assistant;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ai_assistant TO role_ai_assistant;
ALTER DEFAULT PRIVILEGES IN SCHEMA ai_assistant GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO role_ai_assistant;
ALTER DEFAULT PRIVILEGES IN SCHEMA ai_assistant GRANT USAGE, SELECT ON SEQUENCES TO role_ai_assistant;
GRANT role_ai_assistant, role_model, role_audit TO svc_ai_assistant;
