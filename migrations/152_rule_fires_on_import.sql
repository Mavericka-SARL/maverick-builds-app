-- A record rule (form_submit, form_approval) may leave out the records a file
-- import creates: moving existing data into a table started one approval per
-- row — eight "New restaurant opening" workflows for the eight restaurants a
-- chain already runs (2026-10-31). On by default: a rule fires for imported
-- records as it always has until its developer switches this off.
ALTER TABLE workflow.automation_rule
    ADD COLUMN IF NOT EXISTS fires_on_import boolean NOT NULL DEFAULT true;
