-- A dashboard's selector bar: one selector per chosen dimension, drawn once
-- at the top of the dashboard, which every widget on it follows.
--
-- Without it the dashboard's one selector per dimension was drawn by
-- whichever synced widget rendered first, so on a page whose widgets share a
-- dimension (a scenario, a period) the selector could sit on a widget that
-- had nothing to do with it (found 2026-10-16, the scenario dashboards of the
-- shared planning models).
--
-- Each entry is {"dimension_id": "<id>", "default": "<member code>"} in
-- display order; "default" may be left out (the dimension's first member).
-- Dimension ids are revision-scoped, so every copy of a dashboard (revision
-- duplication, model export and import) remaps them like the dimension ids in
-- widget_props; a member code rename re-keys "default".
ALTER TABLE model.dashboard_def
    ADD COLUMN IF NOT EXISTS selector_bar JSONB NOT NULL DEFAULT '[]'::jsonb;
