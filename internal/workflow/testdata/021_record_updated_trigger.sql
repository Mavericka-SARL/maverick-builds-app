-- Mirrors migrations/199_record_updated_trigger.sql for this package's test schema.
ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'record_updated';
ALTER TABLE workflow.automation_rule
    ADD COLUMN IF NOT EXISTS watch_fields text[] NOT NULL DEFAULT '{}';
