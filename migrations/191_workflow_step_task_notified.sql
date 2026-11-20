-- When the people a task or approval is assigned to were told it is theirs
-- (2026-11-20). Until then a step that became someone's task notified no one:
-- with e-mail on, people heard of a run when it was cancelled or reopened,
-- never when it had work for them. NULL = not told yet; a step set back to
-- in_progress (rework, an admin's override) is cleared, so they are told again.
ALTER TABLE workflow.workflow_step
    ADD COLUMN IF NOT EXISTS task_notified_at timestamptz;

-- Tasks open at upgrade are not announced all at once: their people already
-- have them in Pending Actions.
UPDATE workflow.workflow_step SET task_notified_at = now()
WHERE status = 'in_progress' AND task_notified_at IS NULL;
