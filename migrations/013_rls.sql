-- Row-Level Security policies for tenant isolation.
-- Services set the session variable app.user_id before executing queries.
-- Platform admins bypass all policies via the svc_platform_admin role.

-- ── core.workspace ────────────────────────────────────────────────────────────

ALTER TABLE core.workspace ENABLE ROW LEVEL SECURITY;

-- Super-user / service accounts bypass RLS entirely via BYPASSRLS role attribute.
-- Application queries must pass app.user_id via SET LOCAL before executing.

CREATE POLICY workspace_select ON core.workspace
    FOR SELECT
    USING (
        -- Platform admins see all workspaces; others see only their customer's
        current_setting('app.user_id', true) = ''
        OR EXISTS (
            SELECT 1 FROM identity.user u
            WHERE u.id = current_setting('app.user_id', true)::uuid
              AND (
                  u.customer_id = workspace.customer_id
                  OR EXISTS (
                      SELECT 1 FROM identity.role_assignment ra
                      WHERE ra.user_id = u.id
                        AND ra.role = 'platform_admin'
                  )
              )
        )
    );

CREATE POLICY workspace_insert ON core.workspace
    FOR INSERT
    WITH CHECK (true); -- insert validation handled at application layer

CREATE POLICY workspace_update ON core.workspace
    FOR UPDATE
    USING (
        current_setting('app.user_id', true) = ''
        OR EXISTS (
            SELECT 1 FROM identity.user u
            JOIN identity.role_assignment ra ON ra.user_id = u.id
            WHERE u.id = current_setting('app.user_id', true)::uuid
              AND ra.role IN ('platform_admin', 'developer')
        )
    );

-- ── core.application ──────────────────────────────────────────────────────────

ALTER TABLE core.application ENABLE ROW LEVEL SECURITY;

CREATE POLICY application_select ON core.application
    FOR SELECT
    USING (
        current_setting('app.user_id', true) = ''
        OR EXISTS (
            SELECT 1
            FROM core.workspace w
            JOIN identity.user u ON u.customer_id = w.customer_id
            WHERE w.id = application.workspace_id
              AND u.id = current_setting('app.user_id', true)::uuid
        )
        OR EXISTS (
            SELECT 1 FROM identity.role_assignment ra
            WHERE ra.user_id = current_setting('app.user_id', true)::uuid
              AND ra.role = 'platform_admin'
        )
    );

CREATE POLICY application_insert ON core.application
    FOR INSERT
    WITH CHECK (true);

CREATE POLICY application_update ON core.application
    FOR UPDATE
    USING (
        current_setting('app.user_id', true) = ''
        OR EXISTS (
            SELECT 1 FROM identity.role_assignment ra
            WHERE ra.user_id = current_setting('app.user_id', true)::uuid
              AND ra.role IN ('platform_admin', 'developer')
        )
    );

-- ── core.model ────────────────────────────────────────────────────────────────

ALTER TABLE core.model ENABLE ROW LEVEL SECURITY;

CREATE POLICY model_select ON core.model
    FOR SELECT
    USING (
        current_setting('app.user_id', true) = ''
        OR EXISTS (
            SELECT 1
            FROM core.application a
            JOIN core.workspace w ON w.id = a.workspace_id
            JOIN identity.user u ON u.customer_id = w.customer_id
            WHERE a.id = model.application_id
              AND u.id = current_setting('app.user_id', true)::uuid
        )
        OR EXISTS (
            SELECT 1 FROM identity.role_assignment ra
            WHERE ra.user_id = current_setting('app.user_id', true)::uuid
              AND ra.role = 'platform_admin'
        )
    );

CREATE POLICY model_insert ON core.model FOR INSERT WITH CHECK (true);

CREATE POLICY model_update ON core.model
    FOR UPDATE
    USING (
        current_setting('app.user_id', true) = ''
        OR EXISTS (
            SELECT 1 FROM identity.role_assignment ra
            WHERE ra.user_id = current_setting('app.user_id', true)::uuid
              AND ra.role IN ('platform_admin', 'developer')
        )
    );

-- ── Helper function for services ──────────────────────────────────────────────
-- Call core.set_app_user(user_id) at the start of each transaction to activate
-- RLS policies. Pass empty string '' to run as a super-user (migration runner).

CREATE OR REPLACE FUNCTION core.set_app_user(p_user_id TEXT)
RETURNS VOID LANGUAGE plpgsql AS $$
BEGIN
    PERFORM set_config('app.user_id', p_user_id, true); -- true = local (transaction-scoped)
END;
$$;
