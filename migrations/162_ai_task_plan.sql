-- 162: the AI Developer's task plan (2026-11-08, owner: "the AI Developer
-- must be able to plan and split complex tasks"). A goal the developer
-- delegates — build a model from this workbook, add the pages people need —
-- is planned before it is built: stages, each with the items it delivers
-- and how it is checked, worked one proposal at a time. The plan is the
-- session's, kept here so every turn (and the developer) sees where the work
-- stands; NULL for a session without one.
ALTER TABLE ai_assistant.session ADD COLUMN IF NOT EXISTS task_plan jsonb;
