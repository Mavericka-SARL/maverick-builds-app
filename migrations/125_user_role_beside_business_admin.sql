-- The user role (key business_user) is a role of its own: the console's
-- User group (dashboards, the inbox, history, models) comes from it alone,
-- and business_admin no longer stands in for it (web/src/router/sections.ts).
-- So that no business admin loses those screens, every business_admin
-- assignment gets a user assignment beside it, in the same workspace (or
-- none), once. Sign-up grants both from now on (internal/gateway/signup.go).
-- The UNIQUE (user_id, role, workspace_id) constraint does not stop a second
-- row with no workspace, hence the explicit check.
INSERT INTO identity.role_assignment (user_id, role, workspace_id, assigned_by)
SELECT ba.user_id, 'business_user', ba.workspace_id, ba.assigned_by
FROM identity.role_assignment ba
WHERE ba.role = 'business_admin'
  AND NOT EXISTS (
      SELECT 1 FROM identity.role_assignment u
      WHERE u.user_id = ba.user_id AND u.role = 'business_user'
        AND u.workspace_id IS NOT DISTINCT FROM ba.workspace_id);
