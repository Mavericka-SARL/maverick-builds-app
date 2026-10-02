-- A spreadsheet attached to an AI assistant session (.csv, .xlsx, .xlsm) also
-- keeps its original bytes, so the assistant can import the WHOLE file
-- through the same pipeline as the Import Wizard: the extracted text in
-- `content` is capped for the language model (500 rows a sheet, 60k chars)
-- and is not the data. NULL for every other document type, and for
-- spreadsheets attached before this column existed (re-attach to import).
-- Deleted with the document, and with its session (ON DELETE CASCADE).
ALTER TABLE ai_assistant.document ADD COLUMN raw_data BYTEA;
