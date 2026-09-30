// Package assignee draws the boundary of workflow assignment: who a step is
// assigned to, as one SQL predicate that deciding a step (the gRPC
// WorkflowService.CompleteStep, through workflow.Store.IsAssigneeEligible),
// a notification step's role recipients and the SLA reminder all share. It
// imports nothing, so internal/notification (which internal/workflow
// imports) can use it as well as internal/workflow and the gateway.
//
// The gateway's task inbox and step completion use it too: its
// taskAssigneeSQL (internal/gateway/handler.go) is built from SQL, so every
// surface of workflow assignment draws one boundary. The caller's admin scope
// is worked out in SQL from the user's own rows rather than passed in, so the
// predicate also works for a query that enumerates users. A disabled account
// is assigned nothing, and a business role named like a platform role is not
// that role.
package assignee

import "strings"

// SQL returns a predicate, true when the user whose id the SQL expression
// user yields is an assignee of a workflow step in the application whose id
// the SQL expression app yields, which names the roles of the jsonb array
// the SQL expression roles yields (SQL NULL or [] when it names none).
//
//   - A step naming no role is assigned to everyone who reaches the
//     application: a role held in its workspace, or in any workspace of its
//     tenant for a tenant-level application (workspace_id NULL); a tier in
//     its tenant (below); or, for an account with no tenant holding
//     developer or tenant_admin without a workspace, an application or
//     model grant to exactly this application.
//   - A named platform role (identity.role_assignment) counts when it is
//     held in the application's workspace, or in a workspace of its tenant
//     for a tenant-level application. A developer or tenant_admin role
//     held in any workspace of the tenant counts tenant-wide. One held
//     without a workspace counts where its holder holds it: tenant_admin in
//     the account's own tenant, or, on an account with no tenant, in each
//     tenant it holds a workspace role in; developer in the account's own
//     tenant, or, on an account with no tenant, everywhere (a platform-wide
//     builder) — narrowed by application or model grants to exactly the
//     granted applications. An unscoped business role counts nowhere. Only
//     platform_admin counts everywhere.
//   - A named business role (identity.business_role, which lives in a
//     workspace) counts in the applications of its workspace and in the
//     tenant-level applications of its tenant. One named like a platform
//     role (an identity.user_role value) counts nowhere: a step naming
//     tenant_admin means the platform role, and a business admin who named
//     a business role tenant_admin would otherwise make its members
//     assignees of every tenant_admin step of the workspace.
//   - A disabled account (identity.user.disabled_at set) is an assignee of
//     nothing, as the gateway and the gRPC services refuse to resolve it:
//     it is neither notified nor reminded of a step.
//
// The tiers are the gateway's (adminTiers in internal/gateway/admin_tiers.go):
// roles count per tenant, by what the account holds in that tenant only, and
// an application or model grant is no tier anywhere (decided 2026-09-30).
// This predicate used to take the old admin scope, merged across tenants:
// an unscoped tenant_admin of tenant A that held a developer role in a
// workspace of tenant C, or an application grant there, was an assignee —
// and so a decider — of C's steps naming tenant_admin, and of every step of
// every C application naming no role.
//
// Roles that are not a JSON array name nobody: the step is then assigned to
// no one rather than to everyone.
//
// The three expressions may reference the enclosing query's columns and
// parameters, but no alias beginning asg_, which the predicate uses for its
// own tables. Each may appear more than once.
func SQL(app, roles, user string) string {
	return strings.NewReplacer(
		"{app}", "("+app+")",
		"{roles}", "("+roles+")",
		"{user}", "("+user+")",
	).Replace(stepAssigneeSQL)
}

// asg_tenant is the application's tenant: its own customer_id, or its
// workspace's. asg_held is what the user holds that the tiers are read from,
// and asg_tier the tiers themselves in that tenant (adminTiers in
// internal/gateway/admin_tiers.go): an unscoped tenant_admin or developer
// grant that counts here, and in_scope — any tier here, or a customerless
// builder's or administrator's grant to exactly this application.
const stepAssigneeSQL = `EXISTS (
	SELECT 1
	FROM core.application asg_app
	LEFT JOIN core.workspace asg_appws ON asg_appws.id = asg_app.workspace_id
	CROSS JOIN LATERAL (
	    SELECT COALESCE(asg_app.customer_id, asg_appws.customer_id) AS id
	) asg_tenant
	CROSS JOIN LATERAL (
	    SELECT
	        EXISTS (
	            SELECT 1 FROM identity.role_assignment asg_pa
	            WHERE asg_pa.user_id = {user} AND asg_pa.role::text = 'platform_admin'
	        ) AS platform,
	        EXISTS (
	            SELECT 1 FROM identity."user" asg_hu
	            WHERE asg_hu.id = {user} AND asg_hu.customer_id = asg_tenant.id
	        ) AS home,
	        EXISTS (
	            SELECT 1 FROM identity."user" asg_cu
	            WHERE asg_cu.id = {user} AND asg_cu.customer_id IS NULL
	        ) AS customerless,
	        EXISTS (
	            SELECT 1 FROM identity.role_assignment asg_wra
	            JOIN core.workspace asg_wws ON asg_wws.id = asg_wra.workspace_id
	            WHERE asg_wra.user_id = {user} AND asg_wws.customer_id = asg_tenant.id
	        ) AS ws_role,
	        EXISTS (
	            SELECT 1 FROM identity.role_assignment asg_tra
	            JOIN core.workspace asg_tws ON asg_tws.id = asg_tra.workspace_id
	            WHERE asg_tra.user_id = {user} AND asg_tws.customer_id = asg_tenant.id
	              AND asg_tra.role::text IN ('tenant_admin', 'developer')
	        ) AS ws_builder,
	        EXISTS (
	            SELECT 1 FROM identity.role_assignment asg_uta
	            WHERE asg_uta.user_id = {user} AND asg_uta.workspace_id IS NULL
	              AND asg_uta.role::text = 'tenant_admin'
	        ) AS unscoped_tenant_admin,
	        EXISTS (
	            SELECT 1 FROM identity.role_assignment asg_uda
	            WHERE asg_uda.user_id = {user} AND asg_uda.workspace_id IS NULL
	              AND asg_uda.role::text = 'developer'
	        ) AS unscoped_developer,
	        (EXISTS (SELECT 1 FROM identity.user_app_access asg_gua WHERE asg_gua.user_id = {user})
	         OR EXISTS (SELECT 1 FROM identity.user_model_access asg_gum WHERE asg_gum.user_id = {user})) AS has_grants,
	        -- A grant to this application, or — with no application grant,
	        -- which would narrow to those — to one of its models.
	        (EXISTS (
	             SELECT 1 FROM identity.user_app_access asg_ua
	             WHERE asg_ua.user_id = {user} AND asg_ua.application_id = asg_app.id
	         )
	         OR (NOT EXISTS (SELECT 1 FROM identity.user_app_access asg_ua2 WHERE asg_ua2.user_id = {user})
	             AND EXISTS (
	                 SELECT 1 FROM identity.user_model_access asg_um
	                 JOIN core.model asg_umm ON asg_umm.id = asg_um.model_id
	                 WHERE asg_um.user_id = {user} AND asg_umm.application_id = asg_app.id
	             ))) AS granted_app
	) asg_held
	CROSS JOIN LATERAL (
	    SELECT
	        -- An unscoped tenant_admin counts in the account's own tenant, or,
	        -- on an account with no tenant, in each tenant it holds a
	        -- workspace role in; never through a grant.
	        asg_held.unscoped_tenant_admin
	            AND (asg_held.home OR (asg_held.customerless AND asg_held.ws_role)) AS unscoped_tenant_admin_here,
	        -- An unscoped developer counts in the account's own tenant, or,
	        -- on an account with no tenant, everywhere — narrowed by its
	        -- grants to exactly the granted applications.
	        asg_held.unscoped_developer
	            AND (asg_held.home OR (asg_held.customerless AND (NOT asg_held.has_grants OR asg_held.granted_app))) AS unscoped_developer_here
	) asg_unscoped
	CROSS JOIN LATERAL (
	    SELECT (
	        asg_held.platform
	        OR asg_held.ws_builder
	        OR asg_unscoped.unscoped_tenant_admin_here
	        OR asg_unscoped.unscoped_developer_here
	        -- an account with no tenant holding tenant_admin without a
	        -- workspace reaches exactly the applications its grants name
	        OR (asg_held.customerless AND asg_held.unscoped_tenant_admin AND asg_held.granted_app)
	    ) AS in_scope
	) asg_tier
	CROSS JOIN LATERAL (
	    SELECT CASE WHEN jsonb_typeof({roles}) = 'array' THEN {roles} ELSE '[]'::jsonb END AS names
	) asg_roles
	WHERE asg_app.id = {app}
	  -- A disabled account is assigned nothing: it cannot sign in to see
	  -- the task, and must not be told about it.
	  AND EXISTS (
	      SELECT 1 FROM identity."user" asg_live
	      WHERE asg_live.id = {user} AND asg_live.disabled_at IS NULL
	  )
	  AND (
	      -- A step naming no role: everyone who reaches the application.
	      (({roles} IS NULL OR {roles} = '[]'::jsonb)
	       AND (asg_tier.in_scope
	            OR EXISTS (
	                SELECT 1 FROM identity.role_assignment asg_rra
	                JOIN core.workspace asg_rws ON asg_rws.id = asg_rra.workspace_id
	                WHERE asg_rra.user_id = {user}
	                  AND (asg_rws.id = asg_app.workspace_id
	                       OR (asg_app.workspace_id IS NULL AND asg_rws.customer_id = asg_app.customer_id))
	            )))
	      -- A named platform role, held for this application.
	      OR EXISTS (
	          SELECT 1 FROM identity.role_assignment asg_ra
	          LEFT JOIN core.workspace asg_raws ON asg_raws.id = asg_ra.workspace_id
	          WHERE asg_ra.user_id = {user}
	            AND asg_ra.role::text IN (SELECT jsonb_array_elements_text(asg_roles.names))
	            AND (asg_ra.role::text = 'platform_admin'
	                 OR asg_raws.id = asg_app.workspace_id
	                 OR (asg_app.workspace_id IS NULL AND asg_raws.customer_id = asg_app.customer_id)
	                 OR (asg_ra.role::text IN ('developer', 'tenant_admin')
	                     AND asg_raws.customer_id = asg_tenant.id)
	                 OR (asg_ra.workspace_id IS NULL AND asg_ra.role::text = 'tenant_admin'
	                     AND asg_unscoped.unscoped_tenant_admin_here)
	                 OR (asg_ra.workspace_id IS NULL AND asg_ra.role::text = 'developer'
	                     AND asg_unscoped.unscoped_developer_here))
	      )
	      -- A named business role of the application's workspace, or of its
	      -- tenant for a tenant-level application. A business role named like
	      -- a platform role is never that role: the name means the platform
	      -- role only.
	      OR EXISTS (
	          SELECT 1 FROM identity.business_role_member asg_brm
	          JOIN identity.business_role asg_br ON asg_br.id = asg_brm.role_id
	          JOIN core.workspace asg_bws ON asg_bws.id = asg_br.workspace_id
	          WHERE asg_brm.user_id = {user}
	            AND asg_br.name IN (SELECT jsonb_array_elements_text(asg_roles.names))
	            AND asg_br.name NOT IN (SELECT unnest(enum_range(NULL::identity.user_role))::text)
	            AND (asg_bws.id = asg_app.workspace_id
	                 OR (asg_app.workspace_id IS NULL AND asg_bws.customer_id = asg_app.customer_id))
	      )
	  )
)`
