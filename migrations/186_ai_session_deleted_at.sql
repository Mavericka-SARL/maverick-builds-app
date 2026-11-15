-- A deleted AI session is kept for a while instead of gone at once: one
-- click on its trash icon used to DELETE it, and its messages, plan, memory,
-- proposals and attached files with it, past any undo (2026-11-15, an owner's
-- session lost mid-build). deleted_at hides it from the session list and
-- every read of it; it can be restored until ChatStore purges it
-- (DeletedSessionRetention after deleted_at).
ALTER TABLE ai_assistant.session ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS ai_session_deleted_idx
    ON ai_assistant.session (user_id, deleted_at) WHERE deleted_at IS NOT NULL;
