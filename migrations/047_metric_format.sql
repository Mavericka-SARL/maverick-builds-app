-- Add display format to metric definitions.
-- format:          "number" | "percentage" | "currency" | "boolean" | "text"
-- format_decimals: decimal places for number / percentage / currency
-- format_currency: currency symbol (only relevant when format = 'currency')

ALTER TABLE model.metric_def
  ADD COLUMN format          TEXT     NOT NULL DEFAULT 'number',
  ADD COLUMN format_decimals SMALLINT NOT NULL DEFAULT 0,
  ADD COLUMN format_currency TEXT     NOT NULL DEFAULT '$';
