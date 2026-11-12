-- The model a developer chooses for routine AI Developer turns: a turn whose
-- sheet plan the server generated whole (plan_sheet / sheet_plan, every row
-- translated) runs on it; designing the target model, rows the server could
-- not translate and everything else run on the main model. "" (none) keeps
-- every turn on the main model. 2026-11-12: a strong model at high reasoning
-- spent most of a build's calls on mechanical sheet translation.
ALTER TABLE ai_assistant.llm_settings
    ADD COLUMN IF NOT EXISTS routine_model TEXT NOT NULL DEFAULT '';
