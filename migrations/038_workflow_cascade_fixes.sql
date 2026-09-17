-- Fix cascade deletes for workflow tables so deleting an application
-- (or workflow_def) automatically removes dependent instances and steps.

ALTER TABLE workflow.workflow_instance
    DROP CONSTRAINT IF EXISTS workflow_instance_workflow_def_id_fkey,
    ADD CONSTRAINT workflow_instance_workflow_def_id_fkey
        FOREIGN KEY (workflow_def_id) REFERENCES workflow.workflow_def(id)
        ON DELETE CASCADE;

-- Also fix workflow_def → application cascade (in case it's missing).
ALTER TABLE workflow.workflow_def
    DROP CONSTRAINT IF EXISTS workflow_def_application_id_fkey,
    ADD CONSTRAINT workflow_def_application_id_fkey
        FOREIGN KEY (application_id) REFERENCES core.application(id)
        ON DELETE CASCADE;
