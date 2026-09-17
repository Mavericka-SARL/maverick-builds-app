-- Plans, trials and quotas (internal/plan).
--
-- core.customer.plan has named a plan since the first migration and nothing
-- ever read it. From here on a plan is a row: what it allows (limits), whether
-- it starts a trial, and whether a visitor may pick it at self-service
-- sign-up. Limits are data, not code: the platform administrator sets them
-- in the console (Platform › Plans).
--
-- The table lives in the control-plane database (schema platform, like the
-- tenant catalog). Every tenant database runs this migration too and gets
-- the seed rows, but the gateway only ever reads plans from the control
-- plane; the copies inside tenant databases are inert.
CREATE TABLE IF NOT EXISTS platform.plan (
    key          TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    -- A tenant put on this plan gets trial_ends_at = now() + trial_days.
    -- 0 means the plan is not a trial.
    trial_days   INT NOT NULL DEFAULT 0 CHECK (trial_days >= 0 AND trial_days <= 365),
    -- May be chosen at public sign-up (POST /api/signup). The first
    -- self-service plan by sort_order is the one sign-up uses.
    self_service BOOLEAN NOT NULL DEFAULT FALSE,
    -- {"max_users": 5, "max_models": 3, ...}; a missing or zero value means
    -- unlimited. Keys are the JSON names of internal/plan.Limits.
    limits       JSONB NOT NULL DEFAULT '{}'::jsonb,
    sort_order   INT NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO platform.plan (key, name, description, trial_days, self_service, limits, sort_order) VALUES
    ('trial', 'Trial', 'Fourteen days to try the platform with a small model. Read-only once the trial ends.', 14, TRUE,
     '{"max_users": 5, "max_applications": 2, "max_models": 3, "max_metrics_per_model": 50, "max_members_per_dimension": 500,
       "max_fact_rows_per_model": 100000, "max_ai_messages_per_day": 100, "max_integration_runs_per_day": 50}'::jsonb, 10),
    -- The two values existing tenants carry. No limits, so nothing changes
    -- for them; tighten here when a paid tier is priced.
    ('starter',    'Starter',    'The plan tenants created by an administrator start on.', 0, FALSE, '{}'::jsonb, 20),
    ('standard',   'Standard',   'A paid plan. Set its limits here.',                       0, FALSE, '{}'::jsonb, 30),
    ('enterprise', 'Enterprise', 'No limits.',                                              0, FALSE, '{}'::jsonb, 40)
ON CONFLICT (key) DO NOTHING;

ALTER TABLE core.customer
    -- NULL: not on trial. Past: the tenant is read-only until an
    -- administrator changes the plan or extends the date.
    ADD COLUMN IF NOT EXISTS trial_ends_at    TIMESTAMPTZ,
    -- The verdict of the last usage sweep against the plan's limits. 'over'
    -- makes the tenant read-only (deletes stay allowed, so it can get back
    -- under). Written by internal/plan.Sweep, never by a request.
    ADD COLUMN IF NOT EXISTS limit_state      TEXT NOT NULL DEFAULT 'ok' CHECK (limit_state IN ('ok', 'over')),
    ADD COLUMN IF NOT EXISTS limit_reason     TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS usage_checked_at TIMESTAMPTZ;
