-- An approved run superseded by a later approved run of the same workflow
-- and scope (owner decision 2026-11-27): a project's second budget revision
-- is approved over the first instead of waiting for HQ to reopen it — which
-- unlocked the budget for everyone meanwhile. The later run's lock replaces
-- the earlier's when it is approved; until then the earlier keeps the scope
-- locked for everyone but the later run's own freeze. A superseded run stays
-- completed (it was approved), but locks nothing, reports no changes since
-- its approval, and cannot be reopened.
ALTER TABLE workflow.workflow_instance
    ADD COLUMN IF NOT EXISTS superseded_by uuid REFERENCES workflow.workflow_instance(id) ON DELETE SET NULL;
