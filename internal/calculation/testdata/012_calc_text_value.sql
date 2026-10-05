-- A calculated metric of format text (an action key built as ID & "|" &
-- type, a status word) stores its formula's text beside a 0, as a text
-- input's fact does (migration 111). NULL for every number.
ALTER TABLE runtime.calc_result ADD COLUMN IF NOT EXISTS text_value TEXT;
