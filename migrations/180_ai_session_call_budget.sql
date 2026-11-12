-- A call budget the developer sets on one AI Developer session: its turns
-- stop at that many provider calls (NULL: the deployment's
-- AI_MAX_CALLS_PER_SESSION). A build's cost is calls times context; the
-- High-Tech build spent 1,769 calls (2026-11-11) with no way to cap one task.
ALTER TABLE ai_assistant.session ADD COLUMN IF NOT EXISTS call_budget INT CHECK (call_budget IS NULL OR call_budget > 0);
