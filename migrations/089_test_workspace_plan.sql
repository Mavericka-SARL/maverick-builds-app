-- The test workspace: a plan bounded by storage, not by time (internal/plan).
--
-- Self-service sign-up used to hand out a fourteen-day trial with a handful
-- of object counts. From here on it hands out a test workspace: no end
-- date, 100 MB of data, and the two daily caps that bound what a tenant can
-- cost the operator (AI messages, integration runs). The trial plan stays in
-- the catalog for a platform administrator to assign by hand, but is
-- withdrawn from sign-up — SelfService picks the first self_service plan by
-- sort_order, and only one is meant to be offered.
--
-- limit_note is what a tenant is told when a limit stops it. Empty means
-- "change the plan", the engine's words for a deployment whose answer is
-- another plan. The test workspace's answer is elsewhere — run it yourself,
-- or a licence — so its row says that; the wording is data, not code.
--
-- max_storage_mb is exact in a dedicated tenant database and an estimate
-- over the data tables in a shared one (plan.StorageBytes).

ALTER TABLE platform.plan ADD COLUMN IF NOT EXISTS limit_note TEXT NOT NULL DEFAULT '';

INSERT INTO platform.plan (key, name, description, trial_days, self_service, limits, limit_note, sort_order) VALUES
    ('test', 'Test workspace', 'Try the platform for as long as you like, with up to 100 MB of data.', 0, TRUE,
     '{"max_storage_mb": 100, "max_ai_messages_per_day": 100, "max_integration_runs_per_day": 50}'::jsonb,
     'A test workspace holds up to 100 MB. To go further, run the platform on your own infrastructure — free of charge and without limits for non-commercial use, under a commercial licence otherwise — or get the enterprise edition.',
     5)
ON CONFLICT (key) DO NOTHING;

UPDATE platform.plan SET self_service = FALSE, updated_at = now() WHERE key = 'trial' AND self_service;

-- A deleted model takes its cell history with it. 081 archives every deleted
-- fact so that a change which vanished from the record is still on record —
-- for the model it belonged to. Once the model itself is gone no screen can
-- show that history, and keeping it would mean a tenant can never make room
-- by deleting: the archive would grow by exactly what was deleted. So the
-- archive trigger skips facts whose model is being deleted (the cascade runs
-- after the model row is gone), and the model's earlier history goes too.
CREATE OR REPLACE FUNCTION runtime.archive_deleted_fact() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM core.model WHERE id = OLD.model_id) THEN
        RETURN OLD;
    END IF;
    INSERT INTO runtime.fact_input_history
        (id, model_id, revision_id, metric_id, dim_members, value, entered_by, entered_at, source_ref, delete_reason)
    VALUES
        (OLD.id, OLD.model_id, OLD.revision_id, OLD.metric_id, OLD.dim_members, OLD.value, OLD.entered_by, OLD.entered_at, OLD.source_ref,
         COALESCE(current_setting('mvx.delete_reason', true), ''));
    RETURN OLD;
END $$;

CREATE OR REPLACE FUNCTION runtime.drop_history_of_deleted_model() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM runtime.fact_input_history WHERE model_id = OLD.id;
    RETURN OLD;
END $$;

DROP TRIGGER IF EXISTS model_drop_history_on_delete ON core.model;
CREATE TRIGGER model_drop_history_on_delete
    AFTER DELETE ON core.model
    FOR EACH ROW EXECUTE FUNCTION runtime.drop_history_of_deleted_model();
