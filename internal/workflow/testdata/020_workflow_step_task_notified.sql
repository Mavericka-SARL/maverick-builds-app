-- Mirrors migrations/191_workflow_step_task_notified.sql: when a step's people
-- were told it is theirs (Store.NotifyNewTasks).
ALTER TABLE workflow.workflow_step
    ADD COLUMN IF NOT EXISTS task_notified_at timestamptz;
