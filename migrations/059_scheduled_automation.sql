-- Scheduled (cron) automation trigger type. Reuses workflow.automation_rule
-- and workflow.execution rather than a parallel scheduler history: a rule's
-- cron/timezone/misfire/retry config lives on automation_rule, and each
-- fired tick is one workflow.execution row keyed by (rule_id, scheduled_for)
-- for cross-instance idempotency (see internal/workflow/scheduler.go).

ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'schedule';
-- A new enum value cannot be compared against (CHECK constraints, partial
-- index predicates below) in the same transaction it was added in —
-- Postgres error 55P04. Committing here closes that transaction; migrate.Run
-- sends this whole file as one multi-statement string, and an explicit
-- COMMIT mid-string is honored as a real transaction boundary (the standard
-- workaround for this restriction), so everything after is a fresh
-- transaction free to reference 'schedule'.
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

-- A schedule-type rule must carry a cron expression; every other trigger
-- type leaves it NULL.
ALTER TABLE workflow.automation_rule
    ADD CONSTRAINT automation_rule_schedule_cron_chk
        CHECK (trigger_type <> 'schedule' OR cron_expr IS NOT NULL);

CREATE INDEX IF NOT EXISTS automation_rule_due_idx
    ON workflow.automation_rule (next_fire_at)
    WHERE enabled AND trigger_type = 'schedule';

ALTER TABLE workflow.execution
    ADD COLUMN IF NOT EXISTS scheduled_for TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS claimed_by    TEXT;

-- The atomic idempotency/ownership claim for a scheduled tick: an
-- INSERT ... ON CONFLICT DO NOTHING on this index is the claim itself, so
-- two concurrently-polling gateway instances can never both fire the same
-- tick of the same rule.
CREATE UNIQUE INDEX IF NOT EXISTS execution_rule_scheduled_uq
    ON workflow.execution (rule_id, scheduled_for)
    WHERE scheduled_for IS NOT NULL;
