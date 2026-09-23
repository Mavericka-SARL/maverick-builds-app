-- No trials (internal/plan).
--
-- A plan bounds what a tenant may use, never for how long. The hosted
-- service offers one thing at sign-up — the test workspace, 100 MB, no end
-- date (089) — and everything else is a licence key (docs/LICENSING.md) or
-- running the community edition yourself. So the trial primitive goes: the
-- plan's trial_days, the tenant's trial_ends_at, and the 'trial' plan row
-- that 089 had already withdrawn from sign-up. A tenant a platform
-- administrator had put on it by hand moves to the test workspace, which is
-- what it was trying anyway; nothing becomes read-only by this migration.

UPDATE core.customer SET plan = 'test', updated_at = now()
WHERE plan = 'trial' AND EXISTS (SELECT 1 FROM platform.plan WHERE key = 'test');

DELETE FROM platform.plan WHERE key = 'trial';

ALTER TABLE platform.plan DROP COLUMN IF EXISTS trial_days;
ALTER TABLE core.customer DROP COLUMN IF EXISTS trial_ends_at;
