package gateway

// Administration counts per tenant (decided 2026-09-30).
//
// An account's roles used to be merged across every tenant it held one in,
// and read against every tenant it held tenant_admin or developer in: a
// tenant_admin grant in tenant A plus a developer grant in tenant C made its
// holder C's administrator, and an application grant made a tenant's
// administrator elsewhere an administrator of the application's tenant. Now
// the caller's tier in a tenant is decided by the roles it holds in that
// tenant only:
//
//   - tenant_admin: a tenant_admin grant in a workspace of the tenant; an
//     unscoped one on an account that belongs to the tenant; or an unscoped
//     one on an account with no tenant, in each tenant it holds any
//     workspace role in (a platform admin's creation, found live on
//     2026-09-10, which only a platform admin extends:
//     customerlessAdminGainsTenant).
//   - developer: otherwise, a developer grant in a workspace of the tenant, or
//     an unscoped one on an account that belongs to the tenant. A
//     platform-wide builder (platformWideBuilderSQL) is a developer of every
//     tenant.
//
// An application or model grant (identity.user_app_access,
// user_model_access) is no tier anywhere: it opens exactly that application
// or model. For an account with no tenant that holds tenant_admin it is that
// account's reach there (customerlessAdminGrantTenantSQL), never
// administration of the tenant's people. A platform admin is unchanged: every
// tenant, every role.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
)

// The administration tiers adminTiers records per tenant.
const (
	tierTenantAdmin = "tenant_admin"
	tierDeveloper   = "developer"
)

// adminTiers is what an account administers, tenant by tenant.
type adminTiers struct {
	// platform is a platform admin: every tenant, every role.
	platform bool
	// all is every tenant in scope: a platform admin, or a platform-wide
	// builder (a developer of every tenant).
	all bool
	// tier is, per tenant (customer_id), tierTenantAdmin or tierDeveloper.
	tier map[string]string
}

// adminTiersSQL reads the tiers of the account $1, one row per tenant: its
// id and whether the tier there is tenant_admin (else developer).
const adminTiersSQL = `
	SELECT customer_id::text, bool_or(role = 'tenant_admin')
	FROM (
	    -- An unscoped grant — the "platform role" of the Users panel — is
	    -- held in the tenant the account belongs to.
	    SELECT u.customer_id, ra.role::text AS role
	    FROM identity."user" u
	    JOIN identity.role_assignment ra ON ra.user_id = u.id AND ra.workspace_id IS NULL
	    WHERE u.id = $1::uuid AND u.customer_id IS NOT NULL
	      AND ra.role IN ('tenant_admin', 'developer')
	    UNION ALL
	    -- A workspace-scoped grant is held in that workspace's tenant.
	    SELECT w.customer_id, ra.role::text
	    FROM identity.role_assignment ra
	    JOIN core.workspace w ON w.id = ra.workspace_id
	    WHERE ra.user_id = $1::uuid AND ra.role IN ('tenant_admin', 'developer')
	    UNION ALL
	    -- An unscoped tenant_admin on an account with no tenant is held in
	    -- each tenant it holds any workspace role in.
	    SELECT w.customer_id, 'tenant_admin'
	    FROM identity.role_assignment ra
	    JOIN core.workspace w ON w.id = ra.workspace_id
	    JOIN identity."user" u ON u.id = ra.user_id
	    WHERE ra.user_id = $1::uuid AND u.customer_id IS NULL
	      AND EXISTS (SELECT 1 FROM identity.role_assignment ta
	                  WHERE ta.user_id = $1::uuid AND ta.role = 'tenant_admin' AND ta.workspace_id IS NULL)
	) held
	WHERE customer_id IS NOT NULL
	GROUP BY customer_id`

// loadAdminTiers reads a's tiers.
func (h *handler) loadAdminTiers(ctx context.Context, a *actor) (adminTiers, error) {
	t := adminTiers{tier: map[string]string{}}
	if a == nil {
		return t, nil
	}
	if a.hasRole("platform_admin") {
		t.platform, t.all = true, true
		return t, nil
	}
	t.all = h.isGlobalBuilder(ctx, a)
	rows, err := h.db.Query(ctx, adminTiersSQL, a.UserID)
	if err != nil {
		return adminTiers{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var admin bool
		if err := rows.Scan(&id, &admin); err != nil {
			return adminTiers{}, err
		}
		t.tier[id] = tierDeveloper
		if admin {
			t.tier[id] = tierTenantAdmin
		}
	}
	return t, rows.Err()
}

// ids is every tenant the account holds a tier in, sorted. Meaningless when
// all is set.
func (t adminTiers) ids() []string {
	out := make([]string, 0, len(t.tier))
	for id := range t.tier {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// adminIDs is every tenant the account holds tenant_admin in, sorted.
func (t adminTiers) adminIDs() []string {
	out := []string{}
	for id, tier := range t.tier {
		if tier == tierTenantAdmin {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// inScope reports whether the account holds a tier in customerID.
func (t adminTiers) inScope(customerID string) bool {
	return t.all || (customerID != "" && t.tier[customerID] != "")
}

// rolesIn is the account's tier in customerID as the roles assignableRoles,
// roleIsAssignableBy, canGrantWorkspaceRole and canManageResourceAccess
// read: what it may grant, revoke and change there. "" names no tenant: an
// account with none, which only a caller whose scope is every tenant holds a
// tier over.
func (t adminTiers) rolesIn(customerID string) []string {
	switch {
	case t.platform:
		return []string{"platform_admin"}
	case customerID != "" && t.tier[customerID] == tierTenantAdmin:
		return []string{"tenant_admin"}
	case customerID != "" && t.tier[customerID] == tierDeveloper, t.all:
		return []string{"developer"}
	}
	return nil
}

// tenantAdminOf reports whether the account administers customerID as its
// tenant admin (or is a platform admin).
func (t adminTiers) tenantAdminOf(customerID string) bool {
	return t.platform || (customerID != "" && t.tier[customerID] == tierTenantAdmin)
}

// adminRouteKey marks a request that arrived through an administrator-only
// route — adm, tenantAdm and the gates built on them (Handler). There the
// caller acts as a tenant or platform admin and nothing else: a developer
// tier opens nothing (adminCanAccessApp, reachSQL). Without it a tenant
// admin of one tenant that is a developer of another renamed and deleted the
// other tenant's applications (2026-09-29, "A developer grant counts as
// admin scope").
type adminRouteKey struct{}

// adminRoute marks fn's requests as arriving through an administrator-only
// route (adminRouteKey), which is a builder route too (builderRouteKey).
func adminRoute(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fn(w, r.WithContext(context.WithValue(withBuilderRoute(r.Context()), adminRouteKey{}, true)))
	}
}

// onAdminRoute reports whether ctx is an administrator-only route's.
func onAdminRoute(ctx context.Context) bool {
	v, _ := ctx.Value(adminRouteKey{}).(bool)
	return v && onBuilderRoute(ctx) && !onDeveloperRoute(ctx)
}

// customerlessAdminGrantTenantSQL is a SQL predicate, true when the account
// whose id is userExpr has no tenant of its own, holds tenant_admin without a
// workspace, and has an explicit application or model grant into the tenant
// tenantExpr. Such an account — a platform admin's creation — reaches there
// exactly what it was granted: every caller narrows to the granted
// applications and models, as they narrow every account's reach. It
// administers nothing there: its grants give it no tier (adminTiers). An
// application's tenant is its own customer_id or its workspace's.
//
// A tenant_admin grant held inside a workspace is that workspace's tenant's
// administration and reaches nothing elsewhere: counting it here made
// another tenant's administrator with no tenant of its own, added to
// tenant 1 as a business user and granted app1, app1's administrator — it
// renamed app1 and exported its model with the data (2026-09-30).
func customerlessAdminGrantTenantSQL(userExpr, tenantExpr string) string {
	return fmt.Sprintf(`EXISTS (
		    SELECT 1 FROM identity."user" cag_u
		    WHERE cag_u.id = %[1]s AND cag_u.customer_id IS NULL
		      AND EXISTS (SELECT 1 FROM identity.role_assignment cag_ra
		                  WHERE cag_ra.user_id = cag_u.id AND cag_ra.role = 'tenant_admin'
		                    AND cag_ra.workspace_id IS NULL)
		      AND %[2]s IN (
		          SELECT COALESCE(cag_a.customer_id, cag_aw.customer_id)
		          FROM identity.user_app_access cag_ua
		          JOIN core.application cag_a ON cag_a.id = cag_ua.application_id
		          LEFT JOIN core.workspace cag_aw ON cag_aw.id = cag_a.workspace_id
		          WHERE cag_ua.user_id = cag_u.id
		          UNION
		          SELECT COALESCE(cag_ma.customer_id, cag_mw.customer_id)
		          FROM identity.user_model_access cag_um
		          JOIN core.model cag_m ON cag_m.id = cag_um.model_id
		          JOIN core.application cag_ma ON cag_ma.id = cag_m.application_id
		          LEFT JOIN core.workspace cag_mw ON cag_mw.id = cag_ma.workspace_id
		          WHERE cag_um.user_id = cag_u.id
		      )
		)`, userExpr, tenantExpr)
}

// grantedAppSQL is a SQL predicate, true when the application and model
// grants of the account whose id is userExpr name application app (a
// core.application alias): a grant to it, or — when the account holds no
// application grant, which would narrow it to those — a grant to one of its
// models.
func grantedAppSQL(app, userExpr string) string {
	return fmt.Sprintf(`(EXISTS (SELECT 1 FROM identity.user_app_access ga_ua
		        WHERE ga_ua.user_id = %[2]s AND ga_ua.application_id = %[1]s.id)
		    OR (NOT EXISTS (SELECT 1 FROM identity.user_app_access ga_any WHERE ga_any.user_id = %[2]s)
		        AND EXISTS (SELECT 1 FROM identity.user_model_access ga_um JOIN core.model ga_m ON ga_m.id = ga_um.model_id
		                    WHERE ga_um.user_id = %[2]s AND ga_m.application_id = %[1]s.id)))`, app, userExpr)
}

// workspaceGrantableRoles is what a caller whose tier in a workspace's
// tenant is tierRoles (adminTiers.rolesIn) may grant inside the workspace —
// by the checks POST /api/admin/users/{id}/roles and an invitation make
// there — so the users screen offers exactly those.
func workspaceGrantableRoles(act *actor, tierRoles []string) []string {
	out := []string{}
	for _, r := range assignableRoles(tierRoles) {
		if roleIsAssignableBy(act.Roles, r) && canGrantWorkspaceRole(tierRoles, r) {
			out = append(out, r)
		}
	}
	return out
}

// unscopedGrantableRoles is what the caller may grant, with no workspace, to
// an account whose home tenant is customerID ("" = none) — by the checks
// POST /api/admin/users/{id}/roles makes: only on an account that is the
// caller's (a platform admin's, any), by the caller's tier in the account's
// tenant, and never one that would make an account with no tenant a builder
// or administrator of every tenant (platformWideGrantErr).
func unscopedGrantableRoles(act *actor, t adminTiers, customerID string) []string {
	out := []string{}
	if !act.hasRole("platform_admin") && !accountIsCallers(customerID, t.all, t.ids()) {
		return out
	}
	for _, r := range assignableRoles(t.rolesIn(customerID)) {
		if roleIsAssignableBy(act.Roles, r) && platformWideGrantErr(act, r, "", customerID, t.all, t.ids()) == nil {
			out = append(out, r)
		}
	}
	return out
}
