-- Phase 2: AI proposal table for propose→confirm→execute flow.
-- Each proposal stores an ordered list of write steps as JSONB.
-- Steps are executed atomically on confirm; status tracks lifecycle.

CREATE TABLE IF NOT EXISTS ai_assistant.proposal (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id   UUID        NOT NULL REFERENCES ai_assistant.session(id) ON DELETE CASCADE,
    steps        JSONB       NOT NULL DEFAULT '[]',
    status       TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'confirmed', 'rejected', 'executed', 'partial')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    executed_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS proposal_session_status
    ON ai_assistant.proposal (session_id, status);
