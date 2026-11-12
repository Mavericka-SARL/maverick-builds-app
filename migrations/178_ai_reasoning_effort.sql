-- How much an OpenAI reasoning model reasons on each AI Developer call, per
-- developer: "" (the model's own default), none, minimal, low, medium, high
-- or xhigh. Some models refuse function tools on chat completions unless it is
-- "none" (gpt-6-luna answered 400, 2026-11-11), and a lower effort shortens
-- long build turns. The deployment-wide AI_OPENAI_REASONING_EFFORT applies
-- where a developer set none.
ALTER TABLE ai_assistant.llm_settings
    ADD COLUMN IF NOT EXISTS reasoning_effort TEXT NOT NULL DEFAULT '';
