-- Rework loops: a route back to an earlier step re-activates it (and resets
-- what follows it). rework_count records how many times a step was sent
-- back within one instance; the engine stops an instance that loops more
-- than a fixed number of times.
ALTER TABLE workflow.workflow_step
    ADD COLUMN IF NOT EXISTS rework_count INT NOT NULL DEFAULT 0;
