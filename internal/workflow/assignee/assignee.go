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
//     application: a role that opens it — a role held in its workspace, or
//     in any workspace of its tenant for a tenant-level application
//     (workspace_id NULL); a developer role held in any workspace of its
//     tenant; an unscoped developer grant of an account of its tenant — or
//     an admin scope that covers its tenant.
//   - A named platform role (identity.role_assignment) counts when it is
//     held in the application's workspace, or in a workspace of its tenant
//     for a tenant-level application. A developer or tenant_admin role
//     held in any workspace of the tenant counts tenant-wide; an unscoped
//     one counts within its holder's admin scope (the account's own tenant,
//     or the tenants of its explicit app/model grants). An unscoped
//     business role counts nowhere. Only platform_admin counts everywhere.
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
// The admin scope is adminScopeCustomerIDs': platform-wide for a
// platform_admin role or an unscoped developer grant of an account with no
// tenant (a global builder); otherwise the tenant of an account holding an
// unscoped tenant_admin/developer grant, the tenants of the workspaces it
// holds a tenant_admin/developer role in, the tenants of every workspace
// role of an account with no tenant that holds an unscoped
// tenant_admin/developer grant, and the tenants of the applications and
// models a tenant_admin/developer was granted explicitly.
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
// workspace's. asg_admin.in_scope is whether the user's admin scope covers
// that tenant (adminScopeCustomerIDs in internal/gateway/handler.go).
const stepAssigneeSQL = `EXISTS (
	SELECT 1
	FROM core.application asg_app
	LEFT JOIN core.workspace asg_appws ON asg_appws.id = asg_app.workspace_id
	CROSS JOIN LATERAL (
	    SELECT COALESCE(asg_app.customer_id, asg_appws.customer_id) AS id
	) asg_tenant
	CROSS JOIN LATERAL (
	    SELECT (
	        EXISTS (
	            SELECT 1 FROM identity.role_assignment asg_pa
	            WHERE asg_pa.user_id = {user} AND asg_pa.role::text = 'platform_admin'
	        )
	        OR EXISTS (
	            SELECT 1 FROM identity."user" asg_gu
	            JOIN identity.role_assignment asg_gra ON asg_gra.user_id = asg_gu.id
	            WHERE asg_gu.id = {user} AND asg_gu.customer_id IS NULL
	              AND asg_gra.role::text = 'developer' AND asg_gra.workspace_id IS NULL
	        )
	        OR EXISTS (
	            SELECT 1 FROM identity."user" asg_ou
	            WHERE asg_ou.id = {user} AND asg_ou.customer_id = asg_tenant.id
	              AND EXISTS (
	                  SELECT 1 FROM identity.role_assignment asg_ora
	                  WHERE asg_ora.user_id = asg_ou.id AND asg_ora.workspace_id IS NULL
	                    AND asg_ora.role::text IN ('tenant_admin', 'developer')
	              )
	        )
	        OR EXISTS (
	            SELECT 1 FROM identity.role_assignment asg_wra
	            JOIN core.workspace asg_wws ON asg_wws.id = asg_wra.workspace_id
	            WHERE asg_wra.user_id = {user} AND asg_wws.customer_id = asg_tenant.id
	              AND asg_wra.role::text IN ('tenant_admin', 'developer')
	        )
	        OR EXISTS (
	            SELECT 1 FROM identity.role_assignment asg_nra
	            JOIN core.workspace asg_nws ON asg_nws.id = asg_nra.workspace_id
	            JOIN identity."user" asg_nu ON asg_nu.id = asg_nra.user_id
	            WHERE asg_nra.user_id = {user} AND asg_nu.customer_id IS NULL
	              AND asg_nws.customer_id = asg_tenant.id
	              AND EXISTS (
	                  SELECT 1 FROM identity.role_assignment asg_nra2
	                  WHERE asg_nra2.user_id = {user} AND asg_nra2.workspace_id IS NULL
	                    AND asg_nra2.role::text IN ('tenant_admin', 'developer')
	              )
	        )
	        OR (EXISTS (
	                SELECT 1 FROM identity.role_assignment asg_xra
	                WHERE asg_xra.user_id = {user} AND asg_xra.role::text IN ('tenant_admin', 'developer')
	            )
	            AND (EXISTS (
	                     SELECT 1 FROM identity.user_app_access asg_ua
	                     JOIN core.application asg_uaa ON asg_uaa.id = asg_ua.application_id
	                     WHERE asg_ua.user_id = {user} AND asg_uaa.customer_id = asg_tenant.id
	                 )
	                 OR EXISTS (
	                     SELECT 1 FROM identity.user_model_access asg_um
	                     JOIN core.model asg_umm ON asg_umm.id = asg_um.model_id
	                     JOIN core.application asg_uma ON asg_uma.id = asg_umm.application_id
	                     WHERE asg_um.user_id = {user} AND asg_uma.customer_id = asg_tenant.id
	                 )))
	    ) AS in_scope
	) asg_admin
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
	       AND (asg_admin.in_scope
	            OR EXISTS (
	                SELECT 1 FROM identity.role_assignment asg_rra
	                JOIN core.workspace asg_rws ON asg_rws.id = asg_rra.workspace_id
	                WHERE asg_rra.user_id = {user}
	                  AND (asg_rws.id = asg_app.workspace_id
	                       OR (asg_app.workspace_id IS NULL AND asg_rws.customer_id = asg_app.customer_id)
	                       OR (asg_rra.role::text = 'developer' AND asg_rws.customer_id = asg_tenant.id))
	            )
	            OR EXISTS (
	                SELECT 1 FROM identity."user" asg_du
	                JOIN identity.role_assignment asg_dra ON asg_dra.user_id = asg_du.id
	                     AND asg_dra.role::text = 'developer' AND asg_dra.workspace_id IS NULL
	                WHERE asg_du.id = {user} AND asg_du.customer_id = asg_tenant.id
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
	                 OR (asg_ra.workspace_id IS NULL AND asg_ra.role::text IN ('developer', 'tenant_admin')
	                     AND asg_admin.in_scope))
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
