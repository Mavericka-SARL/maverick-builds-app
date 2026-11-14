-- Sheet maps: how a block of an attached sheet stands for model cells, in the
-- import's own terms (reshape + column_map). check_sheet compares a sheet
-- with the model cell by cell through them, and the server's check after a
-- confirmed plan replays them. An executed import saves the map it loaded
-- the sheet with (source 'load'), so the map that loads a sheet is the map
-- that checks it; check_sheet saves the maps the AI gives it (source
-- 'check'), e.g. a block of FY totals beside the months.
--
-- Kept per model, not per revision: a map describes the file's layout and
-- outlives the draft it was made in. The last check's counts are kept with
-- the revision they were read in.
CREATE TABLE IF NOT EXISTS ai_assistant.sheet_map (
    model_id         UUID        NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    file_name        TEXT        NOT NULL,
    sheet            TEXT        NOT NULL DEFAULT '',
    block            TEXT        NOT NULL DEFAULT 'main',
    map              JSONB       NOT NULL,
    source           TEXT        NOT NULL DEFAULT 'check' CHECK (source IN ('load', 'check')),
    revision_id      UUID,
    compared         INT         NOT NULL DEFAULT 0,
    matched          INT         NOT NULL DEFAULT 0,
    mapping_problems INT         NOT NULL DEFAULT 0,
    checked_at       TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS sheet_map_block_uq
    ON ai_assistant.sheet_map (model_id, lower(file_name), lower(sheet), lower(block));

GRANT SELECT, INSERT, UPDATE, DELETE ON ai_assistant.sheet_map TO role_ai_assistant;
