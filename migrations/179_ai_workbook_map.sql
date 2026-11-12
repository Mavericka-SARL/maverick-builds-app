-- The workbook map: which metric each row of an attached workbook became,
-- and how well it matched the last time it was compared. compare_with_sheet
-- writes it; the AI Developer reads it in every turn and every new session
-- (read tool workbook_map, the turn's brief), the server's check after a
-- confirmed plan compares its rows again, and the developer sees it in the
-- chat. In the High-Tech build (2026-11-11) the AI re-listed metrics and
-- re-read sheets every turn to find what a row had become, and each new
-- session started without knowing which rows were done.
--
-- Rows are kept per revision; a metric is named, not referenced, so a map
-- outlives the draft it was made in (a new draft reads its source's map).
CREATE TABLE IF NOT EXISTS ai_assistant.workbook_map (
    model_id     UUID        NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    revision_id  UUID        NOT NULL REFERENCES model.revision(id) ON DELETE CASCADE,
    file_name    TEXT        NOT NULL,
    sheet        TEXT        NOT NULL DEFAULT '',
    row_number   INT         NOT NULL,
    members_key  TEXT        NOT NULL DEFAULT '',
    members      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    metric_name  TEXT        NOT NULL,
    first_column TEXT        NOT NULL,
    label        TEXT        NOT NULL DEFAULT '',
    matched      INT         NOT NULL DEFAULT 0,
    compared     INT         NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (revision_id, file_name, sheet, row_number, members_key)
);

CREATE INDEX IF NOT EXISTS workbook_map_model_file_idx ON ai_assistant.workbook_map (model_id, file_name, updated_at DESC);

GRANT SELECT, INSERT, UPDATE, DELETE ON ai_assistant.workbook_map TO role_ai_assistant;
