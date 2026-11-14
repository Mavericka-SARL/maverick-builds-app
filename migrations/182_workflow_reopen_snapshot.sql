-- Reopening an approved run (2026-11-13, consolidation kit).
--
-- Who may reopen it: role names, matched as start_roles are
-- (internal/workflow/assignee). Empty keeps the earlier rule — the
-- application's business admins and developers (owner decision 2026-11-13);
-- a group close lists the CFO, so the controller who prepared it cannot undo
-- the release alone.
ALTER TABLE workflow.workflow_def ADD COLUMN IF NOT EXISTS reopen_roles jsonb NOT NULL DEFAULT '[]'::jsonb;

-- A reopened run keeps the status cancelled (what it locked is released, as
-- before) and says when, by whom and why it was reopened, so history and the
-- status widget read "reopened", not "cancelled".
ALTER TABLE workflow.workflow_instance
  ADD COLUMN IF NOT EXISTS reopened_at timestamptz,
  ADD COLUMN IF NOT EXISTS reopened_by uuid,
  ADD COLUMN IF NOT EXISTS reopen_reason text;

-- The figures an approved run approved (owner decision 2026-11-13): when a
-- run that locks a scope (its Dimension member fields) finishes approved, the
-- inputs and calculated values in that scope are kept here. A lock leaves
-- model-wide inputs editable (a tax rate, a posting account); when one moves
-- an approved figure, the workflow and the dashboards say "changed since
-- approval" by comparing the current values with these.
CREATE TABLE IF NOT EXISTS workflow.approval_snapshot (
  instance_id uuid NOT NULL REFERENCES workflow.workflow_instance(id) ON DELETE CASCADE,
  revision_id uuid NOT NULL,
  kind        text NOT NULL CHECK (kind IN ('input', 'calc')),
  metric_id   uuid NOT NULL,
  dim_members jsonb NOT NULL,
  value       numeric,
  text_value  text,
  PRIMARY KEY (instance_id, kind, metric_id, dim_members)
);
