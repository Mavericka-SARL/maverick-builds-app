-- Three plans, named like the editions: Community, Commercial and
-- Enterprise (2026-10-06).
--
--   Community   was the Basic workspace ("test"): where a self-service
--               sign-up lands. Its one limit is 100 MB of data; the AI-message
--               and integration-run limits it carried go.
--   Commercial  was Standard. Tenants on Starter — which had no limits
--               either — move to it, and it is where a tenant an
--               administrator creates starts.
--   Enterprise  unchanged.
--
-- Starter goes, and so does Trial, unused since migration 090; a tenant
-- still naming Trial moves to Community, the self-service plan it was.
-- Keys are renamed in place, so each plan keeps whatever the platform
-- administrator had set on it. A tenant names its plan in two places: its
-- own row and the platform's tenant directory.

UPDATE core.customer SET plan = CASE plan
    WHEN 'test'     THEN 'community'
    WHEN 'trial'    THEN 'community'
    WHEN 'standard' THEN 'commercial'
    WHEN 'starter'  THEN 'commercial'
  END
WHERE plan IN ('test', 'trial', 'standard', 'starter');
ALTER TABLE core.customer ALTER COLUMN plan SET DEFAULT 'commercial';

UPDATE platform.tenant_database SET plan = CASE plan
    WHEN 'test'     THEN 'community'
    WHEN 'trial'    THEN 'community'
    WHEN 'standard' THEN 'commercial'
    WHEN 'starter'  THEN 'commercial'
  END
WHERE plan IN ('test', 'trial', 'standard', 'starter');
ALTER TABLE platform.tenant_database ALTER COLUMN plan SET DEFAULT 'commercial';

UPDATE platform.plan
SET key = 'community', name = 'Community',
    limits = limits - 'max_ai_messages_per_day' - 'max_integration_runs_per_day',
    limit_note = replace(limit_note, 'A basic workspace holds', 'A Community workspace holds'),
    updated_at = now()
WHERE key = 'test';

UPDATE platform.plan SET key = 'commercial', name = 'Commercial', updated_at = now()
WHERE key = 'standard';

UPDATE platform.plan SET name = 'Enterprise', updated_at = now()
WHERE key = 'enterprise' AND name <> 'Enterprise';

DELETE FROM platform.plan WHERE key IN ('starter', 'trial');
