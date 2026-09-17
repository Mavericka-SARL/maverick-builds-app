-- Mirror of migration 059 for the in-process test database.
ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'schedule';
-- See migrations/059_scheduled_automation.sql for why this COMMIT is here:
-- a new enum value can't be compared against (CHECK/partial index below) in
-- the same transaction it was added in.
COMMIT;

ALTER TABLE workflow.automation_rule
    ADD COLUMN IF NOT EXISTS cron_expr             TEXT,
    ADD COLUMN IF NOT EXISTS timezone              TEXT NOT NULL DEFAULT 'UTC',
    ADD COLUMN IF NOT EXISTS misfire_policy        TEXT NOT NULL DEFAULT 'skip',
    ADD COLUMN IF NOT EXISTS max_retries           INT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS retry_backoff_seconds INT  NOT NULL DEFAULT 60,
    ADD COLUMN IF NOT EXISTS next_fire_at          TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_fire_at          TIMESTAMPTZ;

ALTER TABLE workflow.automation_rule
    ADD CONSTRAINT automation_rule_misfire_policy_chk
        CHECK (misfire_policy IN ('skip', 'fire_now'));

ALTER TABLE workflow.automation_rule
    ADD CONSTRAINT automation_rule_schedule_cron_chk
        CHECK (trigger_type <> 'schedule' OR cron_expr IS NOT NULL);

CREATE INDEX IF NOT EXISTS automation_rule_due_idx
    ON workflow.automation_rule (next_fire_at)
    WHERE enabled AND trigger_type = 'schedule';

ALTER TABLE workflow.execution
    ADD COLUMN IF NOT EXISTS scheduled_for TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS claimed_by    TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS execution_rule_scheduled_uq
    ON workflow.execution (rule_id, scheduled_for)
    WHERE scheduled_for IS NOT NULL;

-- This test schema's identity.user (testdata/001_schema.sql) is a reduced
-- copy of the production table (migrations/002_identity.sql) missing
-- keycloak_sub/display_name — production already has both, so this is
-- test-only. Store.ensureSystemUser (internal/workflow/scheduler.go) needs
-- them to upsert a real "system" actor row for scheduled fires.
ALTER TABLE identity.user
    ADD COLUMN IF NOT EXISTS keycloak_sub TEXT UNIQUE,
    ADD COLUMN IF NOT EXISTS display_name TEXT NOT NULL DEFAULT '';
