-- Workspaces nobody uses (internal/dormancy). A plan may tell a quiet
-- tenant's administrators that it will be deleted, then delete it: warn after
-- dormant_warn_days without use, delete at dormant_delete_days. 0 and 0, every
-- plan's default, means never; the platform administrator sets them per plan.
ALTER TABLE platform.plan
    ADD COLUMN IF NOT EXISTS dormant_warn_days   INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dormant_delete_days INT NOT NULL DEFAULT 0;
ALTER TABLE platform.plan ADD CONSTRAINT plan_dormancy_periods CHECK (
    (dormant_warn_days = 0 AND dormant_delete_days = 0)
    OR (dormant_warn_days > 0 AND dormant_delete_days > dormant_warn_days AND dormant_delete_days <= 3650));

-- When a dedicated tenant was last used by its people or API clients — a
-- request routed to it, not its own scheduled work and not a platform
-- administrator looking at it — and when its administrators were told it
-- will be deleted (cleared by the next use). Existing tenants start counting
-- now, so none is deleted sooner than a full period after this release.
ALTER TABLE platform.tenant_database
    ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dormancy_warned_at TIMESTAMPTZ;
UPDATE platform.tenant_database SET last_used_at = now() WHERE last_used_at IS NULL;
