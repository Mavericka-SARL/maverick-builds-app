-- Test-schema mirror of model.form_metric_mapping's model, revision and
-- target columns (migrations 035 and later), which the input reads now check
-- to set a records-fed metric's typed values aside (writeguard.FedMetricsCTE).
ALTER TABLE model.form_metric_mapping ADD COLUMN IF NOT EXISTS model_id UUID;
ALTER TABLE model.form_metric_mapping ADD COLUMN IF NOT EXISTS revision_id UUID;
ALTER TABLE model.form_metric_mapping ADD COLUMN IF NOT EXISTS target_metric_id UUID;
