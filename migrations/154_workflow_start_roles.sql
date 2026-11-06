-- Who may start a workflow: role names, matched as a step's assignee_roles
-- are (internal/workflow/assignee). Empty, anyone who reaches the
-- application may start it, as before. Found 2026-11-06 checking a
-- restaurant FP&A model role by role: the CFO and every driver owner could
-- start the month-end close and the budget round, which FP&A runs.
ALTER TABLE workflow.workflow_def ADD COLUMN IF NOT EXISTS start_roles jsonb NOT NULL DEFAULT '[]'::jsonb;
