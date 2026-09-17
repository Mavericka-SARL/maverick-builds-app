-- AI Developer sessions get a human name: auto-generated from the first
-- request (LLM-suggested, truncation fallback) and renameable by the user.
-- Empty string means "not yet named" — the client falls back to the date.
ALTER TABLE ai_assistant.session ADD COLUMN IF NOT EXISTS title TEXT NOT NULL DEFAULT '';
