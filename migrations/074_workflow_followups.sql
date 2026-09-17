-- Follow-ups from the 2026-09-13 workflow scenario run.
--
-- Integration events become real triggers: a rule can fire when an
-- integration run completes or fails, optionally scoped to one integration
-- (source_integration_id NULL = any integration of the application). The
-- trigger catalog had advertised these events for months without anything
-- dispatching them.
--
-- single_active_instance: whether a definition allows only one running
-- instance per dimension-member scope (the historical behaviour, right for
-- "submit this department's budget", wrong for "expense request"). Default
-- keeps today's behaviour.

ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'integration_completed';
ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'integration_failed';

ALTER TABLE workflow.automation_rule
    ADD COLUMN IF NOT EXISTS source_integration_id UUID;

ALTER TABLE workflow.workflow_def
    ADD COLUMN IF NOT EXISTS single_active_instance BOOLEAN NOT NULL DEFAULT true;
