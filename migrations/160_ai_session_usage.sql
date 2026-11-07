-- The tokens an AI Developer session was billed for (2026-11-07): a build's
-- cost was visible only in the gateway log, line by line, and lost when it
-- rotated. Each call adds its provider-reported usage here; the session list
-- shows it, and AI_SESSION_TOKEN_LIMIT can stop a session at a budget.
ALTER TABLE ai_assistant.session
    ADD COLUMN IF NOT EXISTS llm_calls          integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS input_tokens       bigint  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS output_tokens      bigint  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cache_read_tokens  bigint  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cache_write_tokens bigint  NOT NULL DEFAULT 0;
