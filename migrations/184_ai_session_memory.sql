-- The AI Developer's memory of one session (owner, 2026-11-13: "AI developer
-- must have memory"): what it learned from each attachment, read once and
-- never again unless the developer asks; its own notes; and the summary of
-- the earlier conversation once the memory filled up and was compacted. The
-- session goes on as long as the developer works in it.
--
-- earlier_plans are the session's plans that are no longer under way: one
-- plan per request, so a new request sets the open one aside (or follows a
-- finished one), and either can be taken up again.
ALTER TABLE ai_assistant.session ADD COLUMN IF NOT EXISTS memory JSONB;
ALTER TABLE ai_assistant.session ADD COLUMN IF NOT EXISTS earlier_plans JSONB;
