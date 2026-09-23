-- Deleting a user must never fail because they once did their job, and must
-- never take the tenant's data with them.
--
-- Seven foreign keys to identity.user had no ON DELETE rule, so the admin
-- "delete user" endpoint answered 500 for anyone who had ever entered a
-- fact, posted a form record, started a workflow, imported a file or opened
-- the assistant — in production, every real user. What they produced is
-- the tenant's: it stays, and its author becomes "a former user" (NULL),
-- the convention audit.audit_event.actor_user_id and storage.object
-- .created_by already follow. Assistant sessions are the person's own and
-- go with them, as their notifications and API keys already do.

-- Facts (partitioned by entered_at: the parent's constraint and NOT NULL
-- recurse into every partition, including ones created later).
ALTER TABLE runtime.fact_input ALTER COLUMN entered_by DROP NOT NULL;
ALTER TABLE runtime.fact_input
    DROP CONSTRAINT fact_input_entered_by_fkey,
    ADD CONSTRAINT fact_input_entered_by_fkey
        FOREIGN KEY (entered_by) REFERENCES identity.user(id) ON DELETE SET NULL;
-- SET NULL updates every fact the person entered; without an index that is
-- a scan of the whole table per partition.
CREATE INDEX IF NOT EXISTS fact_input_entered_by_idx ON runtime.fact_input (entered_by);

-- Form records.
ALTER TABLE runtime.form_record ALTER COLUMN created_by DROP NOT NULL;
ALTER TABLE runtime.form_record
    DROP CONSTRAINT form_record_created_by_fkey,
    ADD CONSTRAINT form_record_created_by_fkey
        FOREIGN KEY (created_by) REFERENCES identity.user(id) ON DELETE SET NULL;

-- Workflow instances (a running one keeps running: its steps are assigned
-- by role, and a step pinned to the person becomes unassigned, which the
-- inbox already treats as "anyone eligible").
ALTER TABLE workflow.workflow_instance ALTER COLUMN started_by DROP NOT NULL;
ALTER TABLE workflow.workflow_instance
    DROP CONSTRAINT workflow_instance_started_by_fkey,
    ADD CONSTRAINT workflow_instance_started_by_fkey
        FOREIGN KEY (started_by) REFERENCES identity.user(id) ON DELETE SET NULL;
ALTER TABLE workflow.workflow_step
    DROP CONSTRAINT workflow_step_assignee_user_id_fkey,
    ADD CONSTRAINT workflow_step_assignee_user_id_fkey
        FOREIGN KEY (assignee_user_id) REFERENCES identity.user(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS workflow_step_assignee_user_id_idx ON workflow.workflow_step (assignee_user_id);

-- Workflow definitions (authorship only; both columns were already nullable).
ALTER TABLE workflow.workflow_def
    DROP CONSTRAINT workflow_def_created_by_fkey,
    ADD CONSTRAINT workflow_def_created_by_fkey
        FOREIGN KEY (created_by) REFERENCES identity.user(id) ON DELETE SET NULL,
    DROP CONSTRAINT workflow_def_updated_by_fkey,
    ADD CONSTRAINT workflow_def_updated_by_fkey
        FOREIGN KEY (updated_by) REFERENCES identity.user(id) ON DELETE SET NULL;

-- Import jobs.
ALTER TABLE import.import_job ALTER COLUMN created_by DROP NOT NULL;
ALTER TABLE import.import_job
    DROP CONSTRAINT import_job_created_by_fkey,
    ADD CONSTRAINT import_job_created_by_fkey
        FOREIGN KEY (created_by) REFERENCES identity.user(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS import_job_created_by_idx ON import.import_job (created_by);

-- Assistant sessions (messages, actions, proposals and documents already
-- cascade from the session).
ALTER TABLE ai_assistant.session
    DROP CONSTRAINT session_user_id_fkey,
    ADD CONSTRAINT session_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES identity.user(id) ON DELETE CASCADE;
