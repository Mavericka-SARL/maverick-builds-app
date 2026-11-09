-- 167: platform-wide reach is a marker, not the absence of rows (owner's
-- decision, 2026-11-08). An account with no tenant and an unscoped developer
-- grant built in every tenant exactly when it had no application or model
-- grant, so deleting its last grant — a revoke, an application's or a
-- tenant's deletion — widened a narrowed developer to every tenant unless
-- that path also revoked its developer grant (private observations,
-- 2026-09-30). Now identity.user.platform_wide must be set as well:
--   - an unscoped developer grant to an account with no tenant and no
--     application or model grant sets it — the grant a platform admin makes
--     a platform-wide builder with (only a platform admin may);
--   - any application or model grant clears it: the account is narrowed,
--     and stays narrowed when the grants go.
-- Accounts that are platform-wide today keep it.
ALTER TABLE identity."user" ADD COLUMN IF NOT EXISTS platform_wide BOOLEAN NOT NULL DEFAULT false;

UPDATE identity."user" u SET platform_wide = true
WHERE u.customer_id IS NULL
  AND EXISTS (SELECT 1 FROM identity.role_assignment ra
              WHERE ra.user_id = u.id AND ra.role = 'developer' AND ra.workspace_id IS NULL)
  AND NOT EXISTS (SELECT 1 FROM identity.user_app_access ua WHERE ua.user_id = u.id)
  AND NOT EXISTS (SELECT 1 FROM identity.user_model_access um WHERE um.user_id = u.id);

CREATE OR REPLACE FUNCTION identity.mark_platform_wide() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE identity."user" u SET platform_wide = true
    WHERE u.id = NEW.user_id AND u.customer_id IS NULL AND NOT u.platform_wide
      AND NOT EXISTS (SELECT 1 FROM identity.user_app_access ua WHERE ua.user_id = u.id)
      AND NOT EXISTS (SELECT 1 FROM identity.user_model_access um WHERE um.user_id = u.id);
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS unscoped_developer_marks_platform_wide ON identity.role_assignment;
CREATE TRIGGER unscoped_developer_marks_platform_wide
    AFTER INSERT ON identity.role_assignment
    FOR EACH ROW WHEN (NEW.role = 'developer' AND NEW.workspace_id IS NULL)
    EXECUTE FUNCTION identity.mark_platform_wide();

CREATE OR REPLACE FUNCTION identity.narrow_platform_wide() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE identity."user" SET platform_wide = false WHERE id = NEW.user_id AND platform_wide;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS app_grant_narrows_platform_wide ON identity.user_app_access;
CREATE TRIGGER app_grant_narrows_platform_wide
    AFTER INSERT ON identity.user_app_access
    FOR EACH ROW EXECUTE FUNCTION identity.narrow_platform_wide();

DROP TRIGGER IF EXISTS model_grant_narrows_platform_wide ON identity.user_model_access;
CREATE TRIGGER model_grant_narrows_platform_wide
    AFTER INSERT ON identity.user_model_access
    FOR EACH ROW EXECUTE FUNCTION identity.narrow_platform_wide();
