-- Daily limits on AI Developer calls, set by the people whose key pays for
-- them (owner, 2026-11-16): a developer's own (AI Developer settings), and a
-- tenant admin's for every developer of the tenant (AI keys, enterprise).
-- The lower of those set counts; with neither, the server's default
-- (AI_MAX_CALLS_PER_DAY), which also stays the ceiling for calls on the
-- server's own key. NULL is no limit set.
ALTER TABLE ai_assistant.llm_settings
    ADD COLUMN IF NOT EXISTS daily_call_limit INT CHECK (daily_call_limit IS NULL OR daily_call_limit > 0);
ALTER TABLE ai_assistant.tenant_llm_settings
    ADD COLUMN IF NOT EXISTS daily_call_limit INT CHECK (daily_call_limit IS NULL OR daily_call_limit > 0);
