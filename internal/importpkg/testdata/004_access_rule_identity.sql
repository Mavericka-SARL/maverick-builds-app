-- Test-schema mirror of migrations/099_access_rule_identity.sql: the lineage
-- columns the access-rule resolver (writeguard.RulesForRevision) reads. The
-- backfill is left out; these tests start from empty tables, and every row
-- gets its own lineage from the column default.
ALTER TABLE model.dimension_def    ADD COLUMN lineage_id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE model.dimension_member ADD COLUMN lineage_id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE model.metric_def       ADD COLUMN lineage_id UUID NOT NULL DEFAULT gen_random_uuid();

ALTER TABLE identity.user_access_rule
    ADD COLUMN IF NOT EXISTS id UUID NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN ref_lineage_id UUID;

-- The resolver scopes dimensions and metrics by revision_id (NULL = every
-- revision); this schema's copies of those tables predate the column.
ALTER TABLE model.dimension_def ADD COLUMN IF NOT EXISTS revision_id UUID;
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS revision_id UUID;
