-- An account belongs to the tenant of what it holds (decided 2026-09-30).
--
-- A platform admin's invitation used to leave the new account with no
-- tenant (identity.user customer_id NULL), even when it named the workspace
-- the account was given its role in; the gateway now sets the workspace's
-- tenant. This gives the accounts made before it theirs: an account with no
-- tenant whose every role — a role in a workspace, a membership of a
-- workspace's business role, an application or model grant (an
-- application's tenant is its own customer_id or its workspace's) — lies in
-- one and the same tenant belongs to that tenant from here on.
--
-- Left as they are: a platform admin; an account holding developer or
-- tenant_admin without a workspace, which on an account with no tenant means
-- something else (a platform-wide builder, or a builder narrowed to its
-- grants, or an administrator of every tenant it holds a role in) and is a
-- platform admin's to decide; and an account whose roles lie in several
-- tenants, or in none.
--
-- In database-per-tenant mode (pkg/tenantdb) one identity (keycloak_sub)
-- can have a user row in the control plane and in any number of tenant
-- databases, and each database is migrated on its own. A tenant's database
-- holds that tenant's workspaces only, so "every role in one tenant" cannot
-- be told there: every account in it would pass. So nothing is written in a
-- tenant's database — the one named tenant_<its customer id without dashes>
-- (tenantdb.DatabaseName) — and in the control plane an identity the
-- directory (platform.user_directory) lists in a tenant database is left
-- alone too: it holds roles there this database does not see.
--
-- Only accounts with no tenant are written, so the migration is safe to
-- re-run and a no-op on an empty database.
WITH held AS (
    SELECT ra.user_id, w.customer_id
    FROM identity.role_assignment ra
    JOIN core.workspace w ON w.id = ra.workspace_id
    UNION
    SELECT brm.user_id, w.customer_id
    FROM identity.business_role_member brm
    JOIN identity.business_role br ON br.id = brm.role_id
    JOIN core.workspace w ON w.id = br.workspace_id
    UNION
    SELECT ua.user_id, COALESCE(a.customer_id, aw.customer_id)
    FROM identity.user_app_access ua
    JOIN core.application a ON a.id = ua.application_id
    LEFT JOIN core.workspace aw ON aw.id = a.workspace_id
    UNION
    SELECT um.user_id, COALESCE(a.customer_id, aw.customer_id)
    FROM identity.user_model_access um
    JOIN core.model m ON m.id = um.model_id
    JOIN core.application a ON a.id = m.application_id
    LEFT JOIN core.workspace aw ON aw.id = a.workspace_id
), one_tenant AS (
    SELECT user_id, (array_agg(customer_id))[1] AS customer_id
    FROM held
    GROUP BY user_id
    HAVING count(DISTINCT customer_id) = 1 AND bool_and(customer_id IS NOT NULL)
)
UPDATE identity."user" u
SET customer_id = o.customer_id
FROM one_tenant o
WHERE u.id = o.user_id
  AND u.customer_id IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM identity.role_assignment ra
      WHERE ra.user_id = u.id
        AND (ra.role = 'platform_admin'
             OR (ra.role IN ('developer', 'tenant_admin') AND ra.workspace_id IS NULL))
  )
  AND NOT EXISTS (
      SELECT 1 FROM platform.user_directory d WHERE d.keycloak_sub = u.keycloak_sub
  )
  AND NOT EXISTS (
      SELECT 1 FROM core.customer c
      WHERE current_database() = 'tenant_' || replace(c.id::text, '-', '')
  );
