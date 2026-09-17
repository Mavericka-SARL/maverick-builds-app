-- Test runs become a property of the instance instead of a context flag.
--
-- POST /api/developer/workflows/{id}/test-run used to stuff
-- context["_test_mode"] = "true" into an otherwise completely ordinary
-- StartWorkflow call. Nothing ever read that key — grep found the write site
-- and the generated OpenAPI comments, and no reader — so a "test" run was a
-- real instance: it dispatched real notifications to real recipients, ran
-- on_approve fact copies against real revisions, and appeared in every
-- instance list and task inbox like any other.
--
-- test_run is set once at start and never changes, so every side-effect site
-- can consult the instance rather than having to thread a mode through the
-- call graph.
ALTER TABLE workflow.workflow_instance
    ADD COLUMN IF NOT EXISTS test_run BOOLEAN NOT NULL DEFAULT false;

-- Test runs are excluded from the operational instance lists far more often
-- than they're selected for, so index the exception.
CREATE INDEX IF NOT EXISTS workflow_instance_test_run_idx
    ON workflow.workflow_instance (workflow_def_id)
    WHERE test_run;
