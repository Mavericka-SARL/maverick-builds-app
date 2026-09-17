-- Documents uploaded into an AI assistant session. The extracted plain text
-- (not the original binary) is stored and injected into the LLM context on
-- each chat call for that session.
CREATE TABLE IF NOT EXISTS ai_assistant.document (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id   UUID        NOT NULL REFERENCES ai_assistant.session(id) ON DELETE CASCADE,
    filename     TEXT        NOT NULL,
    mime_type    TEXT        NOT NULL DEFAULT '',
    char_count   INT         NOT NULL DEFAULT 0,
    truncated    BOOLEAN     NOT NULL DEFAULT false,
    content      TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS document_session_idx ON ai_assistant.document (session_id, created_at);
