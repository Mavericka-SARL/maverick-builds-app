-- AI Assistant chat layer: messages, proposals (Phase 2), and per-user LLM settings.
-- Extends the existing ai_assistant.session table from migration 016.

-- Extend sessions with the LLM provider choice made at session start.
ALTER TABLE ai_assistant.session
    ADD COLUMN IF NOT EXISTS llm_provider TEXT NOT NULL DEFAULT 'openai',
    ADD COLUMN IF NOT EXISTS llm_model    TEXT NOT NULL DEFAULT 'gpt-4o-mini';

-- Full conversation history: every user and assistant turn.
CREATE TABLE IF NOT EXISTS ai_assistant.message (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id  UUID        NOT NULL REFERENCES ai_assistant.session(id) ON DELETE CASCADE,
    role        TEXT        NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
    content     TEXT        NOT NULL DEFAULT '',
    -- tool_calls: array of {id, name, arguments} the assistant wants to invoke
    tool_calls  JSONB,
    -- tool_call_id: set on role='tool' messages to link back to the call
    tool_call_id TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS message_session_idx ON ai_assistant.message (session_id, created_at);

-- Per-developer LLM settings: provider, model, and optionally an encrypted API key.
-- api_key_enc is AES-256-GCM encrypted; NULL means use the platform-level key.
CREATE TABLE IF NOT EXISTS ai_assistant.llm_settings (
    user_id      UUID        PRIMARY KEY REFERENCES identity.user(id) ON DELETE CASCADE,
    provider     TEXT        NOT NULL DEFAULT 'openai',
    model        TEXT        NOT NULL DEFAULT 'gpt-4o-mini',
    api_key_enc  TEXT,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Grant new tables to the existing ai_assistant role.
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_assistant.message     TO role_ai_assistant;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_assistant.llm_settings TO role_ai_assistant;
