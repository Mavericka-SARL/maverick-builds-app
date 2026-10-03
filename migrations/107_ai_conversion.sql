-- A spreadsheet attached to an AI Assistant session, converted to the
-- layout an import reads: the attachment plus how it is reshaped and
-- column-mapped (internal/importpkg.Reshape). The converted file itself is
-- not stored — a download rebuilds it from the attachment, as CSV or Excel.
-- It goes with its session and with the attachment it converts.
CREATE TABLE IF NOT EXISTS ai_assistant.conversion (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id  UUID        NOT NULL REFERENCES ai_assistant.session(id) ON DELETE CASCADE,
    document_id UUID        NOT NULL REFERENCES ai_assistant.document(id) ON DELETE CASCADE,
    sheet       TEXT        NOT NULL DEFAULT '',
    reshape     JSONB,
    column_map  JSONB,
    filename    TEXT        NOT NULL,
    row_count   INT         NOT NULL DEFAULT 0,
    columns     TEXT[]      NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS conversion_session_idx ON ai_assistant.conversion (session_id, created_at);
