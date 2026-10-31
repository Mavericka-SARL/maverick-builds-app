-- Mirrors migrations/152_rule_fires_on_import.sql for this package's test schema.
ALTER TABLE workflow.automation_rule
    ADD COLUMN IF NOT EXISTS fires_on_import boolean NOT NULL DEFAULT true;
