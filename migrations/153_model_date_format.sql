-- How a model writes dates for its readers: "" (the reader's browser, as
-- before), DD/MM/YYYY, MM/DD/YYYY, YYYY-MM-DD, DD.MM.YYYY or D MMM YYYY.
-- A developer sets it; grid cells, tiles, record lists and the date fields
-- people type into follow it. Asked for 2026-11-05: a transaction date read
-- 03/05/2026 in a US-English browser where the model's users read 5 March.
ALTER TABLE core.model
    ADD COLUMN IF NOT EXISTS date_format text NOT NULL DEFAULT '';
