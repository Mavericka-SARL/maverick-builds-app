-- A rule that fires when a record of a table changes: a rescheduled activity
-- starts the check of the contracts that end before it, instead of waiting
-- for a weekly schedule (development-programme build, 2026-11-27). The
-- event is record_updated; watch_fields, when not empty, narrows it to a
-- change of one of the named fields (empty: any field). A rule's other
-- settings (source_form_id, fires_on_import, conditions) apply as for
-- form_submit.
ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'record_updated';
ALTER TABLE workflow.automation_rule
    ADD COLUMN IF NOT EXISTS watch_fields text[] NOT NULL DEFAULT '{}';
