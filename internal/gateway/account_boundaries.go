package gateway

// Account boundaries for the users screen (decided 2026-09-30).
//
// An account has a home: its identity.user customer_id. An administrator who
// is not a platform admin changes an account itself — renames, deletes,
// disables, re-invites it, changes its e-mail — only when that home is one of
// the tenants the administrator holds a tier in (adminTiers), it is not a
// platform admin, and the administrator's tier THERE may revoke every role it
// holds. An account of another tenant, or of none, that holds roles in the
// workspaces of a tenant the administrator is tenant admin of is listed, and
// can be removed from that tenant: what it holds there goes, nothing else
// about it changes. Inviting an address that already has an account adds it
// to the tenant the same way, inside a workspace, and leaves the account as
// it is.
//
// accountPermissionsFor is the one rule: the users list reports it to the
// console, and each of these mutations is refused by it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Where an account's home (identity.user customer_id) is, seen from the
// calling administrator's tenants.
const (
	homeOwn   = "own"   // one of the caller's tenants
	homeOther = "other" // another tenant
	homeNone  = "none"  // no tenant of its own
)

// accountFacts is what accountPermissionsFor reads about one account.
type accountFacts struct {
	ID            string
	Email         string
	CustomerID    string // "" = none
	PlatformAdmin bool
	PlatformWide  bool // platformWideBuilderSQL
	Self          bool // the caller's own account
	// ScopeRoles are the roles the account holds in workspaces of the
	// tenants the caller is tenant admin of (adminTiers.adminIDs): what
	// Remove from this tenant takes.
	ScopeRoles []string
	// HoldsInScope is whether the account holds anything there — a
	// workspace role, a business-role membership, an application or model
	// grant — that Remove from this tenant would take (removeTenantAccess).
	HoldsInScope bool
	// Roles are every role the account holds, in any workspace or none.
	Roles []string
}

// accountPermissions says which account-level actions the caller may take on
// one account. It is the "permissions" object of GET /api/admin/users.
type accountPermissions struct {
	Rename           bool `json:"rename"`
	Delete           bool `json:"delete"`
	Disable          bool `json:"disable"`
	Reinvite         bool `json:"reinvite"`
	RemoveFromTenant bool `json:"remove_from_tenant"`
}

// homeTenant classifies customerID against the tenants the caller holds a
// tier in (adminTiers.ids). A caller whose scope is every tenant counts every
// account that has a tenant as within it.
func homeTenant(customerID string, scopeAll bool, scope []string) string {
	switch {
	case customerID == "":
		return homeNone
	case scopeAll || slices.Contains(scope, customerID):
		return homeOwn
	default:
		return homeOther
	}
}

// accountIsCallers reports whether an account whose customer_id is
// customerID is the caller's to change (scopeAll and scope are the caller's
// adminTiers all and ids): its home is one of the caller's tenants, or — for a
// caller whose scope is every tenant, a platform-wide builder, whose own
// invitations make accounts of no tenant — it has no tenant at all. Decided
// for tenant admins (2026-09-30); a platform-level builder keeps what it had.
func accountIsCallers(customerID string, scopeAll bool, scope []string) bool {
	return homeTenant(customerID, scopeAll, scope) == homeOwn || (scopeAll && customerID == "")
}

// accountPermissionsFor is the rule for the account-level actions, used by
// the users list and by every mutation it describes. A platform admin keeps
// every action but deleting itself, and removes people role by role (its
// scope is every tenant, so "this tenant" names none). Anyone else changes
// only an account that is its own (accountIsCallers), never a platform admin
// or a platform-wide builder (which only a platform admin modifies), and
// only one whose every role its tier in the account's home tenant may revoke
// (adminTiers.rolesIn, roleIsAssignableBy): deleting an account takes all of
// them, and renaming or re-inviting it is no less its administrator's
// business. A developer could delete its own tenant's administrator,
// identity-provider account and all, though it may not revoke that
// administrator's role (2026-09-30), and a tenant admin elsewhere that is a
// developer of the home tenant read its tenant_admin tier there. Its own
// account it renames and re-invites whatever it holds. It removes from the
// tenants it is tenant admin of an account whose home is none of them — when
// it may revoke every role the account holds there (a tenant_admin grant a
// platform admin made stays).
func accountPermissionsFor(act *actor, tiers adminTiers, f accountFacts) (string, accountPermissions) {
	home := homeTenant(f.CustomerID, tiers.all, tiers.ids())
	if tiers.platform {
		return home, accountPermissions{Rename: true, Delete: !f.Self, Disable: true, Reinvite: true}
	}
	if f.PlatformAdmin || f.PlatformWide {
		return home, accountPermissions{}
	}
	own := accountIsCallers(f.CustomerID, tiers.all, tiers.ids())
	change := own && (f.Self || revokesEvery(tiers.rolesIn(f.CustomerID), f.Roles))
	p := accountPermissions{Rename: change, Delete: change && !f.Self, Disable: change, Reinvite: change}
	admin := tiers.adminIDs()
	p.RemoveFromTenant = !tiers.all && !f.Self && f.HoldsInScope && !slices.Contains(admin, f.CustomerID) &&
		revokesEvery([]string{"tenant_admin"}, f.ScopeRoles)
	return home, p
}

// revokesEvery reports whether a caller whose tier is callerRoles may revoke
// every one of roles.
func revokesEvery(callerRoles, roles []string) bool {
	return !slices.ContainsFunc(roles, func(r string) bool { return !roleIsAssignableBy(callerRoles, r) })
}

// loadAccountFacts reads accountFacts for ids, keyed by the id as the
// database spells it. scope is the tenants the caller is tenant admin of
// (adminTiers.adminIDs), which ScopeRoles is read over.
func (h *handler) loadAccountFacts(ctx context.Context, act *actor, scope, ids []string) (map[string]accountFacts, error) {
	if scope == nil {
		scope = []string{}
	}
	rows, err := h.db.Query(ctx, `
		SELECT u.id::text, u.keycloak_sub, u.email, COALESCE(u.customer_id::text, ''),
		       EXISTS (SELECT 1 FROM identity.role_assignment pa WHERE pa.user_id = u.id AND pa.role = 'platform_admin'),
		       `+platformWideBuilderSQL("u.id")+`,
		       u.id = NULLIF($2, '')::uuid,
		       ARRAY(SELECT DISTINCT ra.role::text FROM identity.role_assignment ra
		             JOIN core.workspace w ON w.id = ra.workspace_id
		             WHERE ra.user_id = u.id AND w.customer_id::text = ANY($3::text[])
		             ORDER BY 1),
		       ARRAY(SELECT DISTINCT ra.role::text FROM identity.role_assignment ra WHERE ra.user_id = u.id ORDER BY 1),
		       EXISTS (SELECT 1 FROM identity.role_assignment ra JOIN core.workspace w ON w.id = ra.workspace_id
		               WHERE ra.user_id = u.id AND w.customer_id::text = ANY($3::text[]))
		       OR EXISTS (SELECT 1 FROM identity.business_role_member brm
		                  JOIN identity.business_role br ON br.id = brm.role_id JOIN core.workspace w ON w.id = br.workspace_id
		                  WHERE brm.user_id = u.id AND w.customer_id::text = ANY($3::text[]))
		       OR EXISTS (SELECT 1 FROM identity.user_app_access ua JOIN core.application a ON a.id = ua.application_id
		                  LEFT JOIN core.workspace aw ON aw.id = a.workspace_id
		                  WHERE ua.user_id = u.id AND COALESCE(a.customer_id, aw.customer_id)::text = ANY($3::text[]))
		       OR EXISTS (SELECT 1 FROM identity.user_model_access um JOIN core.model m ON m.id = um.model_id
		                  JOIN core.application a ON a.id = m.application_id LEFT JOIN core.workspace aw ON aw.id = a.workspace_id
		                  WHERE um.user_id = u.id AND COALESCE(a.customer_id, aw.customer_id)::text = ANY($3::text[]))
		FROM identity."user" u
		WHERE u.id = ANY($1::uuid[])`, ids, act.UserID, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]accountFacts, len(ids))
	subOf := map[string]string{}
	for rows.Next() {
		var f accountFacts
		var sub string
		if err := rows.Scan(&f.ID, &sub, &f.Email, &f.CustomerID, &f.PlatformAdmin, &f.PlatformWide, &f.Self, &f.ScopeRoles, &f.Roles, &f.HoldsInScope); err != nil {
			return nil, err
		}
		out[f.ID] = f
		subOf[f.ID] = sub
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// A tenant's row of someone with platform reach in the control plane is
	// a platform account's, as in one database (platformReachOf).
	subs := make([]string, 0, len(subOf))
	for _, sub := range subOf {
		subs = append(subs, sub)
	}
	reach, err := h.platformReachOf(ctx, subs)
	if err != nil {
		return nil, err
	}
	for id, f := range out {
		if reach[subOf[id]] {
			f.PlatformAdmin = true
			out[id] = f
		}
	}
	return out, nil
}

// accountFactsOf is loadAccountFacts for one account, spelled any way a uuid
// can be (scope as there). found is false when there is no such account.
func (h *handler) accountFactsOf(ctx context.Context, act *actor, scope []string, userID string) (f accountFacts, found bool, err error) {
	facts, err := h.loadAccountFacts(ctx, act, scope, []string{userID})
	if err != nil {
		return accountFacts{}, false, err
	}
	for _, f := range facts {
		return f, true, nil
	}
	return accountFacts{}, false, nil
}

// accountActionRefusal is the refusal of an account-level action on an
// account whose home is not the caller's (accountPermissionsFor).
func accountActionRefusal(home, action string) error {
	if home == homeOther {
		return fmt.Errorf("forbidden: this account belongs to another tenant, so only that tenant's administrators or a platform admin "+
			"can %s it. You can remove it from your tenant instead: that takes away what it holds here and leaves the account as it is", action)
	}
	return fmt.Errorf("forbidden: this account belongs to no tenant, so only a platform admin can %s it. "+
		"You can remove it from your tenant instead: that takes away what it holds here and leaves the account as it is", action)
}

// accountActionAllowed answers the refusal of an account-level action the
// caller may not take on userID (accountPermissionsFor) and returns false.
// allowed picks the action's permission; action names it in the refusal.
func (h *handler) accountActionAllowed(ctx context.Context, w http.ResponseWriter, act *actor, tiers adminTiers,
	userID, action string, allowed func(accountPermissions) bool) bool {
	f, found, err := h.accountFactsOf(ctx, act, tiers.adminIDs(), userID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return false
	}
	if !found {
		jsonErr(w, fmt.Errorf("user not found"), http.StatusNotFound)
		return false
	}
	home, p := accountPermissionsFor(act, tiers, f)
	if allowed(p) {
		return true
	}
	if accountIsCallers(f.CustomerID, tiers.all, tiers.ids()) && !f.PlatformAdmin && !f.PlatformWide {
		tierRoles := tiers.rolesIn(f.CustomerID)
		unrevocable := slices.DeleteFunc(slices.Clone(f.Roles), func(r string) bool { return roleIsAssignableBy(tierRoles, r) })
		jsonErr(w, fmt.Errorf("forbidden: this account holds a role you may not revoke (%s), so you may not %s it either",
			strings.Join(unrevocable, ", "), action), http.StatusForbidden)
		return false
	}
	jsonErr(w, accountActionRefusal(home, action), http.StatusForbidden)
	return false
}

// withUserPermissions fills each listed user's home_tenant and permissions
// for the caller.
func (h *handler) withUserPermissions(ctx context.Context, act *actor, tiers adminTiers, users []adminUserItem) error {
	if len(users) == 0 {
		return nil
	}
	ids := make([]string, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	facts, err := h.loadAccountFacts(ctx, act, tiers.adminIDs(), ids)
	if err != nil {
		return err
	}
	for i := range users {
		f, ok := facts[users[i].ID]
		if !ok {
			// Deleted between the two reads: nothing may be done to it.
			users[i].HomeTenant = homeNone
			users[i].GrantableRoles = []string{}
			continue
		}
		users[i].HomeTenant, users[i].Permissions = accountPermissionsFor(act, tiers, f)
		users[i].GrantableRoles = unscopedGrantableRoles(ctx, act, tiers, f.CustomerID)
	}
	return h.labelMembersFromElsewhere(ctx, users)
}

// labelMembersFromElsewhere marks as homeOther the accounts listed with no
// tenant that another database holds — another dedicated tenant's, or (seen
// from a dedicated tenant) the control plane: members
// (addMemberFromElsewhere, ReconcileAdoptedAccounts), whose row has no
// tenant only because theirs is in another database. They were shown as
// having no tenant of their own.
func (h *handler) labelMembersFromElsewhere(ctx context.Context, users []adminUserItem) error {
	here := tenantdb.TenantFrom(ctx)
	if h.db.Router() == nil {
		return nil
	}
	var ids []string
	for _, u := range users {
		if u.HomeTenant == homeNone {
			ids = append(ids, u.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	subs := map[string]string{} // keycloak_sub -> user id
	rows, err := h.db.Query(ctx, `SELECT id::text, keycloak_sub FROM identity."user" WHERE id::text = ANY($1)`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, sub string
		if err := rows.Scan(&id, &sub); err != nil {
			rows.Close()
			return err
		}
		subs[sub] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	list := make([]string, 0, len(subs))
	for sub := range subs {
		list = append(list, sub)
	}
	elsewhere := map[string]bool{}
	drows, err := h.db.Control().Query(ctx, `
		SELECT keycloak_sub FROM platform.user_directory
		WHERE keycloak_sub = ANY($1) AND customer_id::text <> $2
		UNION
		SELECT keycloak_sub FROM identity."user" WHERE $2 <> '' AND keycloak_sub = ANY($1)`, list, here)
	if err != nil {
		return err
	}
	defer drows.Close()
	for drows.Next() {
		var sub string
		if err := drows.Scan(&sub); err != nil {
			return err
		}
		elsewhere[subs[sub]] = true
	}
	if err := drows.Err(); err != nil {
		return err
	}
	for i := range users {
		if elsewhere[users[i].ID] {
			users[i].HomeTenant = homeOther
		}
	}
	return nil
}

// ── Removing the last grant (decided 2026-09-30) ─────────────────────────────

// revokedGrant is a grant a removal took away with the account's last
// application or model grant, so as not to leave it platform-wide
// (removeGrants).
type revokedGrant struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
}

// revokeUnnarrowed deletes, in tx, the unscoped developer grant of each of
// accounts that is platform-wide now (platformWideBuilderSQL): an account
// with no tenant that held one was narrowed to its application and model
// grants, and the removal in tx took the last of them. That grant is the only
// one that makes such an account platform-wide, and so a developer of every
// tenant (adminTiers); once it is gone the account builds
// nothing. A platform admin's is left alone: its reach is every tenant with
// it or without. It then checks no account is left platform-wide, and fails
// — rolling the removal back — if one is.
func revokeUnnarrowed(ctx context.Context, tx pgx.Tx, accounts []string) ([]revokedGrant, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	const notPlatformAdmin = `NOT EXISTS (SELECT 1 FROM identity.role_assignment rv_pa
		WHERE rv_pa.user_id = %s AND rv_pa.role = 'platform_admin')`
	rows, err := tx.Query(ctx, `
		DELETE FROM identity.role_assignment ra
		USING identity."user" u
		WHERE u.id = ra.user_id AND ra.user_id = ANY($1::uuid[])
		  AND ra.role = 'developer' AND ra.workspace_id IS NULL
		  AND `+fmt.Sprintf(notPlatformAdmin, "ra.user_id")+`
		  AND `+platformWideBuilderSQL("ra.user_id")+`
		RETURNING ra.user_id::text, u.email`, accounts)
	if err != nil {
		return nil, err
	}
	var revoked []revokedGrant
	seen := map[string]bool{}
	for rows.Next() {
		var g revokedGrant
		if err := rows.Scan(&g.UserID, &g.Email); err != nil {
			rows.Close()
			return nil, err
		}
		if !seen[g.UserID] {
			seen[g.UserID] = true
			g.Role = "developer"
			revoked = append(revoked, g)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var left bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM unnest($1::uuid[]) AS acc(id)
		               WHERE `+fmt.Sprintf(notPlatformAdmin, "acc.id")+` AND `+platformWideBuilderSQL("acc.id")+`)`,
		accounts).Scan(&left); err != nil {
		return nil, err
	}
	if left {
		return nil, errors.New("the removal would leave an account platform-wide; nothing was removed")
	}
	return revoked, nil
}

// auditRevoked records each grant revokeUnnarrowed took away as its own
// event, with what caused it.
func (h *handler) auditRevoked(ctx context.Context, act *actor, revoked []revokedGrant, cause map[string]string) {
	for _, g := range revoked {
		meta := map[string]string{"role": g.Role, "workspace_id": "", "email": g.Email,
			"reason": "last_grant_removed: without an application or model grant this account, which has no tenant, " +
				"would have been a builder of every tenant"}
		for k, v := range cause {
			meta[k] = v
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserRoleRevoked,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "identity_user", ResourceID: g.UserID,
			Metadata: meta,
		})
	}
}

// withRevoked adds the grants a removal also took away to its response.
func withRevoked(resp map[string]any, revoked []revokedGrant) map[string]any {
	if len(revoked) > 0 {
		resp["revoked"] = revoked
	}
	return resp
}

// ── Removing an account from the caller's tenant ─────────────────────────────

// tenantAccessRemoval is what DELETE /api/admin/users/{id}/tenant-access took
// away.
type tenantAccessRemoval struct {
	Roles         []string // role@workspace
	BusinessRoles int64
	AppAccess     int64
	ModelAccess   int64
	AccessRules   int64
}

// removeTenantAccess deletes, in tx, what userID holds in the tenants scope:
// its roles in their workspaces, its memberships of their business roles,
// its grants to their applications and models, and its access rules on those
// models' members and metrics. An application's tenant is its own
// customer_id or its workspace's. Nothing else of the account is touched.
func removeTenantAccess(ctx context.Context, tx pgx.Tx, userID string, scope []string) (tenantAccessRemoval, error) {
	var out tenantAccessRemoval
	rows, err := tx.Query(ctx, `
		DELETE FROM identity.role_assignment ra
		USING core.workspace w
		WHERE w.id = ra.workspace_id AND ra.user_id = $1::uuid AND w.customer_id::text = ANY($2::text[])
		RETURNING ra.role::text || '@' || ra.workspace_id::text`, userID, scope)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return out, err
		}
		out.Roles = append(out.Roles, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	const appInScope = `SELECT a.id FROM core.application a LEFT JOIN core.workspace aw ON aw.id = a.workspace_id
		WHERE COALESCE(a.customer_id, aw.customer_id)::text = ANY($2::text[])`
	const modelInScope = `SELECT m.id FROM core.model m WHERE m.application_id IN (` + appInScope + `)`
	const refUUID = `CASE WHEN r.ref_id ~* ` + writeguard.UUIDPatternSQL + ` THEN r.ref_id::uuid END`
	for _, st := range []struct {
		n   *int64
		sql string
	}{
		{&out.BusinessRoles, `DELETE FROM identity.business_role_member brm
			USING identity.business_role br, core.workspace w
			WHERE br.id = brm.role_id AND w.id = br.workspace_id
			  AND brm.user_id = $1::uuid AND w.customer_id::text = ANY($2::text[])`},
		{&out.AppAccess, `DELETE FROM identity.user_app_access
			WHERE user_id = $1::uuid AND application_id IN (` + appInScope + `)`},
		{&out.ModelAccess, `DELETE FROM identity.user_model_access
			WHERE user_id = $1::uuid AND model_id IN (` + modelInScope + `)`},
		// A member or metric rule belongs to the model of the row it names,
		// by id or by lineage in any revision (accessRulesOnOtherModels).
		{&out.AccessRules, `DELETE FROM identity.user_access_rule r
			WHERE r.user_id = $1::uuid AND r.rule_type IN ('dimension_member', 'metric')
			  AND (EXISTS (SELECT 1 FROM model.dimension_member dm JOIN model.dimension_def d ON d.id = dm.dimension_id
			               WHERE r.rule_type = 'dimension_member' AND d.model_id IN (` + modelInScope + `)
			                 AND (dm.id = ` + refUUID + ` OR dm.lineage_id = r.ref_lineage_id))
			       OR EXISTS (SELECT 1 FROM model.metric_def md
			                  WHERE r.rule_type = 'metric' AND md.model_id IN (` + modelInScope + `)
			                    AND (md.id = ` + refUUID + ` OR md.lineage_id = r.ref_lineage_id)))`},
	} {
		tag, err := tx.Exec(ctx, st.sql, userID, scope)
		if err != nil {
			return out, err
		}
		*st.n = tag.RowsAffected()
	}
	return out, nil
}

// adminRemoveFromTenant is DELETE /api/admin/users/{id}/tenant-access: the
// caller takes away everything an account whose home is elsewhere holds in
// the tenants the caller is tenant admin of (removeTenantAccess), in one
// transaction that locks the account. If that was the last application or
// model grant narrowing a developer with no tenant, its unscoped developer
// grant goes too (revokeUnnarrowed). Refused as accountPermissionsFor says.
func (h *handler) adminRemoveFromTenant(ctx context.Context, w http.ResponseWriter, act *actor, tiers adminTiers, userID string) {
	scope := tiers.adminIDs()
	f, found, err := h.accountFactsOf(ctx, act, scope, userID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !found {
		jsonErr(w, fmt.Errorf("user not found"), http.StatusNotFound)
		return
	}
	_, p := accountPermissionsFor(act, tiers, f)
	// Where the account's home is, seen from the tenants the removal is from.
	home := homeTenant(f.CustomerID, false, scope)
	if !p.RemoveFromTenant {
		switch {
		case !tiers.platform && len(scope) == 0:
			jsonErr(w, fmt.Errorf("forbidden: only a tenant admin can remove an account from the tenant"), http.StatusForbidden)
		case tiers.all:
			jsonErr(w, fmt.Errorf("your scope is every tenant, so there is no one tenant to remove this account from: "+
				"revoke its roles and grants one by one, or delete the account"), http.StatusBadRequest)
		case f.Self:
			jsonErr(w, fmt.Errorf("you cannot remove yourself from your tenant; ask another administrator"), http.StatusForbidden)
		case home == homeOwn:
			jsonErr(w, fmt.Errorf("this account belongs to your tenant: delete it, or revoke its roles one by one"), http.StatusBadRequest)
		case !f.HoldsInScope:
			jsonErr(w, fmt.Errorf("this account holds nothing in a tenant you administer, so there is nothing to remove it from"), http.StatusBadRequest)
		default:
			jsonErr(w, fmt.Errorf("forbidden: this account holds a role in your tenant that only a platform admin can remove (%s)",
				strings.Join(f.ScopeRoles, ", ")), http.StatusForbidden)
		}
		return
	}

	tx, err := h.db.Begin(ctx)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Locked as removeGrants locks: a concurrent removal of the same
	// account's grants waits, and then sees these gone.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM identity."user" WHERE id = $1::uuid FOR NO KEY UPDATE`, f.ID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	// Read again under the lock: a role granted since is removed only if the
	// caller may revoke it.
	var unrevocable []string
	if err := tx.QueryRow(ctx, `
		SELECT ARRAY(SELECT DISTINCT ra.role::text FROM identity.role_assignment ra JOIN core.workspace w ON w.id = ra.workspace_id
		             WHERE ra.user_id = $1::uuid AND w.customer_id::text = ANY($2::text[]))`, f.ID, scope).Scan(&unrevocable); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	unrevocable = slices.DeleteFunc(unrevocable, func(r string) bool { return roleIsAssignableBy([]string{"tenant_admin"}, r) })
	if len(unrevocable) > 0 {
		jsonErr(w, fmt.Errorf("forbidden: this account holds a role in your tenant that only a platform admin can remove (%s)",
			strings.Join(unrevocable, ", ")), http.StatusForbidden)
		return
	}
	removed, err := removeTenantAccess(ctx, tx, f.ID, scope)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	revoked, err := revokeUnnarrowed(ctx, tx, []string{f.ID})
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	// In a dedicated tenant's database the removal took everything the
	// member held in it, so the directory stops routing them here: the
	// tenant left their list, empty, and stayed there. The row stays, as in
	// one database, for what they wrote; adding them again restores the
	// entry (addExistingAccount).
	if f.CustomerID == "" && tenantdb.TenantFrom(ctx) != "" {
		var sub string
		if err := h.db.QueryRow(ctx, `SELECT keycloak_sub FROM identity."user" WHERE id = $1::uuid`, f.ID).Scan(&sub); err == nil {
			h.forgetUser(ctx, sub)
		}
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserRoleRevoked,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "identity_user", ResourceID: f.ID,
		Metadata: map[string]string{
			"action":         "removed_from_tenant",
			"email":          f.Email,
			"home_tenant":    home,
			"customer_ids":   strings.Join(scope, ","),
			"roles":          strings.Join(removed.Roles, ","),
			"business_roles": fmt.Sprint(removed.BusinessRoles),
			"app_access":     fmt.Sprint(removed.AppAccess),
			"model_access":   fmt.Sprint(removed.ModelAccess),
			"access_rules":   fmt.Sprint(removed.AccessRules),
		},
	})
	h.auditRevoked(ctx, act, revoked, map[string]string{"cause": "removed_from_tenant"})
	jsonOK(w, withRevoked(map[string]any{"status": "removed"}, revoked))
}

// ── Inviting an address that already has an account ─────────────────────────

// existingAccountID returns the id of the account the application already
// has under email (compared case-insensitively) or identity-provider subject
// sub, preferring the e-mail's, or "". A platform admin's stand-in is no
// account: the control plane holds theirs (identityHeldIn).
func (h *handler) existingAccountID(ctx context.Context, email, sub string) (string, error) {
	var id string
	err := h.db.QueryRow(ctx, `
		SELECT id::text FROM identity.user
		WHERE NOT stand_in AND (($1 <> '' AND lower(email) = lower($1)) OR ($2 <> '' AND keycloak_sub = $2))
		ORDER BY ($1 <> '' AND lower(email) = lower($1)) DESC
		LIMIT 1`, email, sub).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Where, other than the database a request is routed to, an identity has a
// user row (identityHeldIn).
type heldAt int

const (
	heldNowhere heldAt = iota
	// heldInTenants: dedicated tenants' databases only.
	heldInTenants
	// heldInControl: the control plane, where platform admins and every
	// tenant without a database of its own live — whatever else holds it.
	heldInControl
)

// identityHeldIn reports where, other than the database ctx is routed to, a
// user row is held under identity-provider subject sub or — without an
// identity provider — email (compared case-insensitively): the control plane,
// or a dedicated tenant the directory lists. heldSub is that row's subject,
// the one sub names when it matched. Always heldNowhere with a single
// database.
//
// existingAccountID sees the routed database only. With a database per
// tenant, an invitation into a dedicated tenant of a person another database
// held found no account, so it adopted the identity-provider account as a
// failed creation's: a row the inviting tenant owned (renamed, re-invited and
// deleted by its admin), its realm role added to the shared account, a
// set-password e-mail, and a directory entry — which, for someone the control
// plane held, routed every request of theirs to the inviting tenant: a
// platform admin lost the platform console to any tenant admin who invited
// their address (2026-09-30). SCIM creation did the same, and then changed
// the shared account's e-mail, disabled it or deleted it.
func (h *handler) identityHeldIn(ctx context.Context, email, sub string) (held heldAt, heldSub string, err error) {
	// With an identity provider, it alone says whose an address is, and every
	// caller passes the subject it gives for the address; an e-mail a
	// directory entry or a control-plane row still carries can be stale, and
	// refused a new person's address as someone else's (2026-09-30).
	if h.kc != nil {
		email = ""
	}
	if h.db.Router() == nil || (email == "" && sub == "") {
		return heldNowhere, "", nil
	}
	var inControl, inTenants bool
	err = h.db.Control().QueryRow(ctx, `
		WITH held AS (
		    SELECT keycloak_sub, TRUE AS control FROM identity."user"
		    WHERE $3 <> '' AND (($1 <> '' AND lower(email) = lower($1)) OR ($2 <> '' AND keycloak_sub = $2))
		    UNION ALL
		    SELECT keycloak_sub, FALSE FROM platform.user_directory
		    WHERE customer_id::text <> $3
		      AND (($1 <> '' AND lower(email) = lower($1)) OR ($2 <> '' AND keycloak_sub = $2)))
		SELECT COALESCE(bool_or(control), FALSE), COALESCE(bool_or(NOT control), FALSE),
		       COALESCE((SELECT keycloak_sub FROM held ORDER BY keycloak_sub = $2 DESC LIMIT 1), '')
		FROM held`, email, sub, tenantdb.TenantFrom(ctx)).Scan(&inControl, &inTenants, &heldSub)
	switch {
	case err != nil:
		return heldNowhere, "", err
	case inControl:
		return heldInControl, heldSub, nil
	case inTenants:
		return heldInTenants, heldSub, nil
	}
	return heldNowhere, "", nil
}

// heldElsewhereRefusal refuses an address another database holds when the
// identity provider knows it under no subject: whose it is cannot be told.
const heldElsewhereRefusal = "this person's account is in another database, and could not be added here"

// addMemberFromElsewhere is POST /api/admin/users for someone another
// database holds and this one does not — another dedicated tenant's, or the
// control plane, or from the control plane a dedicated tenant's — by a
// caller who is not a platform admin. The person is added as
// addExistingAccount adds an account, not adopted: a user row here under
// their subject that belongs to no tenant (its tenant lives in another
// database), so this tenant's admin cannot rename, re-invite or delete it
// (accountIsCallers); the name and e-mail the identity provider holds for
// them; a directory entry in a dedicated tenant's database, so their
// requests reach it when they address it (the control plane is reached
// through its tenants and applications, tenantRouting); and then the role,
// the notification and the answer addExistingAccount gives. Nothing on the
// shared identity-provider account changes, and no set-password e-mail is
// sent.
func (h *handler) addMemberFromElsewhere(ctx context.Context, w http.ResponseWriter, act *actor, tierRoles []string, sub, email, role, workspaceID string) {
	name := email
	if h.kc != nil {
		if u, err := h.kc.GetUser(ctx, sub); err == nil {
			if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
				name = n
			}
			if u.Email != "" {
				email = u.Email
			}
		}
	}
	var userID string
	if err := h.db.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, $2, $3)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text`, sub, email, name).Scan(&userID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	h.noteUser(ctx, sub, email)
	h.addExistingAccount(ctx, w, act, tierRoles, userID, email, role, workspaceID)
}

// followIdentity brings a member's row in a dedicated tenant's database
// (addMemberFromElsewhere) up to the name and e-mail of the token they signed
// in with. The row is a copy: its tenant's administrators cannot rename it,
// and a change in the person's home database — a SCIM update, their own
// profile — reached it nowhere else. Only a row with no tenant that another
// database holds follows; any other keeps what its administrators gave it.
func (h *handler) followIdentity(ctx context.Context, a *actor, claims *identity.Claims) {
	email := strings.TrimSpace(claims.Email)
	name := strings.TrimSpace(claims.Name)
	if name == "" {
		name = strings.TrimSpace(claims.GivenName + " " + claims.FamilyName)
	}
	if a.CustomerID != "" || h.db.Router() == nil || email == "" || name == "" || reservedAddress(email) ||
		(strings.EqualFold(email, a.Email) && name == a.Name) {
		return
	}
	if held, _, err := h.identityHeldIn(ctx, "", claims.Subject); err != nil || held == heldNowhere {
		return
	}
	if _, err := h.db.Exec(ctx, `
		UPDATE identity."user" SET email = $2, display_name = $3, updated_at = now()
		WHERE id = $1::uuid AND customer_id IS NULL AND NOT stand_in`, a.UserID, email, name); err != nil {
		h.log.Warn().Err(err).Str("user_id", a.UserID).Msg("member's name and e-mail not brought up to their sign-in")
		return
	}
	a.Email, a.Name = email, name
}

// addExistingAccount is POST /api/admin/users for an address that already has
// an account, by a caller who is not a platform admin. Creation used to adopt
// the account — keep its tenant or give it the caller's, overwrite its name,
// add the role, mail it a set-password link — with no check that it was the
// caller's to touch (2026-09-29), and then answered 409. Now it adds the role
// in a workspace of the caller's tenant (checked by the create path: a role
// the caller may grant there) and nothing else: the account's name, e-mail,
// tenant, active flag and identity-provider account stay as they are, and no
// invitation is re-sent. The person is told in the notification centre, and
// by e-mail when the tenant sends notifications by e-mail. The answer is the
// one a new invitation gets, so the flow does not tell whether the address
// had an account.
//
// Whatever cannot be added answers the same, grants nothing and is audited
// for the platform only (refuseExistingAccount): a platform admin's account
// or a platform-wide builder's, which only a platform admin changes; and an
// account with no tenant holding tenant_admin without a workspace, which any
// role in a new tenant's workspace would make that tenant's administrator
// (customerlessAdminGainsTenant). A 400 naming the existing account, and the
// real id of a platform admin's, used to tell the caller the address was
// taken, and whose it was (2026-09-30). A request with no role or no
// workspace, which used to be answered here as done and grant nothing, is
// refused before the address is looked at, alike for a new one
// (errInviteNeedsRoleAndWorkspace). tierRoles is the caller's tier in the
// workspace's tenant (adminTiers).
func (h *handler) addExistingAccount(ctx context.Context, w http.ResponseWriter, act *actor, tierRoles []string, userID, email, role, workspaceID string) {
	var platformAdmin, platformWide bool
	var existingSub string
	if err := h.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM identity.role_assignment WHERE user_id = $1::uuid AND role = 'platform_admin'),
		       `+platformWideBuilderSQL("$1::uuid")+`, (SELECT keycloak_sub FROM identity."user" WHERE id = $1::uuid)`,
		userID).Scan(&platformAdmin, &platformWide, &existingSub); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	// In a tenant's database the control plane says it (platformReachOf).
	if reach, err := h.platformReachOf(ctx, []string{existingSub}); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	} else if reach[existingSub] {
		platformAdmin = true
	}
	if platformAdmin || platformWide {
		h.refuseExistingAccount(ctx, w, act, userID, email, role, workspaceID,
			"a platform admin or platform-wide builder is modified only by a platform admin")
		return
	}
	if !roleIsAssignableBy(tierRoles, "tenant_admin") {
		gains, err := h.customerlessAdminGainsTenant(ctx, userID, workspaceID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if gains {
			h.refuseExistingAccount(ctx, w, act, userID, email, role, workspaceID, customerlessAdminRefusal)
			return
		}
	}
	here := tenantdb.TenantFrom(ctx)
	var sub string
	if here != "" {
		if err := h.db.QueryRow(ctx, `SELECT keycloak_sub FROM identity."user" WHERE id = $1::uuid`, userID).Scan(&sub); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	tag, err := h.db.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id, assigned_by)
		VALUES ($1::uuid, $2::identity.user_role, $3::uuid, (SELECT id FROM identity.user WHERE id = NULLIF($4,'')::uuid))
		ON CONFLICT DO NOTHING`, userID, role, workspaceID, act.UserID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserRoleGranted,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "identity_user", ResourceID: userID,
		Metadata: map[string]string{"email": email, "role": role, "workspace_id": workspaceID,
			"existing_account": "true", "granted": fmt.Sprint(tag.RowsAffected() > 0)},
	})
	if tag.RowsAffected() > 0 {
		h.notifyAccessGranted(ctx, userID, role, workspaceID)
	}
	// A member of a dedicated tenant removed before (adminRemoveFromTenant)
	// is routed here again.
	if here != "" {
		h.noteUser(ctx, sub, email)
	}
	jsonOK(w, map[string]any{"id": userID, "status": "created", "invited": h.kc != nil})
}

// errInviteNeedsRoleAndWorkspace refuses, to anyone but a platform admin, an
// invitation that does not name both a role and a workspace. It is checked
// before the address is looked at, so a new address and an existing account
// are answered alike.
var errInviteNeedsRoleAndWorkspace = errors.New("role and workspace_id are both required: an invitation gives a role " +
	"inside a workspace, and someone who already has an account is added only that way")

// refuseExistingAccount answers an invitation of an existing account that
// adds nothing as a new invitation is answered — under an id that names no
// account, not the real one — and audits the refusal with reason. The event
// is the platform's only (auditPlatformOnly): shown in the caller's tenant,
// it said which addresses had accounts, and which were platform admins'.
func (h *handler) refuseExistingAccount(ctx context.Context, w http.ResponseWriter, act *actor, userID, email, role, workspaceID, reason string) {
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserRoleGranted,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "identity_user", ResourceID: userID,
		Metadata: map[string]string{"email": email, "role": role, "workspace_id": workspaceID,
			"existing_account": "true", "granted": "false", "refused": reason, auditVisibilityKey: auditPlatformOnly},
	})
	jsonOK(w, map[string]any{"id": uuid.NewString(), "status": "created", "invited": h.kc != nil})
}

// An audit event whose metadata has visibility=platform is shown to platform
// admins only, never in a tenant's audit scope (auditScope).
const (
	auditVisibilityKey = "visibility"
	auditPlatformOnly  = "platform"
)

// customerlessAdminRefusal refuses a role that would make an account with no
// tenant administrator of a tenant (customerlessAdminGainsTenant).
const customerlessAdminRefusal = "this account has no tenant and holds tenant_admin without a workspace, so any role " +
	"in a workspace of another tenant makes it that tenant's administrator; only a platform admin can grant that"

// customerlessAdminGainsTenant reports whether a role for userID in
// workspaceID would make it administrator of a tenant it does not administer
// yet. An account with no tenant of its own that holds tenant_admin without a
// workspace administers every tenant it holds any workspace role in
// (adminTiers): even business_user in a new tenant's workspace is
// tenant_admin there, which only a platform admin grants (assignableRoles).
func (h *handler) customerlessAdminGainsTenant(ctx context.Context, userID, workspaceID string) (bool, error) {
	var gains bool
	err := h.db.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM identity."user" u JOIN core.workspace nw ON nw.id = $2::uuid
		    WHERE u.id = $1::uuid AND u.customer_id IS NULL
		      AND EXISTS (SELECT 1 FROM identity.role_assignment ta
		                  WHERE ta.user_id = u.id AND ta.role = 'tenant_admin' AND ta.workspace_id IS NULL)
		      AND NOT EXISTS (SELECT 1 FROM identity.role_assignment ra JOIN core.workspace w ON w.id = ra.workspace_id
		                      WHERE ra.user_id = u.id AND w.customer_id = nw.customer_id))`, userID, workspaceID).Scan(&gains)
	return gains, err
}

// accessGrantedTemplate is the notification an account gets when an
// administrator adds it to a workspace (addExistingAccount).
const accessGrantedTemplate = "access_granted"

// accessGrantedDailyCap is how many access_granted notifications one tenant
// sends one person in a day; one workspace sends it one. Adding an account
// and removing it from the tenant, over and over, mailed it every time.
const accessGrantedDailyCap = 3

// notifyAccessGranted tells userID it was given role in workspaceID, in the
// notification centre and — when the workspace's tenant sends notifications
// by e-mail — by e-mail (notification.Store.Notify). The wording is the
// platform's; the tenant's and the workspace's names, which their
// administrators choose, are quoted, on one line. A failure is logged: the
// grant stands without it.
func (h *handler) notifyAccessGranted(ctx context.Context, userID, role, workspaceID string) {
	var customerID, tenant, workspace string
	var sentByTenant int
	var sentForWorkspace bool
	if err := h.db.QueryRow(ctx, `
		SELECT c.id::text, c.name, w.name,
		       (SELECT count(*) FROM notification.notification n
		        WHERE n.recipient_user_id = $2::uuid AND n.template_id = $3 AND n.channel = 'in_app'
		          AND n.template_vars->>'customer_id' = c.id::text AND n.created_at > now() - interval '1 day'),
		       EXISTS (SELECT 1 FROM notification.notification n
		               WHERE n.recipient_user_id = $2::uuid AND n.template_id = $3 AND n.channel = 'in_app'
		                 AND n.resource_type = 'workspace' AND n.resource_id = w.id::text AND n.created_at > now() - interval '1 day')
		FROM core.workspace w JOIN core.customer c ON c.id = w.customer_id WHERE w.id = $1::uuid`,
		workspaceID, userID, accessGrantedTemplate).Scan(&customerID, &tenant, &workspace, &sentByTenant, &sentForWorkspace); err != nil {
		h.log.Warn().Err(err).Str("workspace_id", workspaceID).Msg("access-granted notification: workspace not found")
		return
	}
	if sentForWorkspace || sentByTenant >= accessGrantedDailyCap {
		h.log.Info().Str("user_id", userID).Str("workspace_id", workspaceID).Msg("access-granted notification not repeated within a day")
		return
	}
	tenant, workspace = oneLineName(tenant), oneLineName(workspace)
	role = strings.ReplaceAll(role, "_", " ")
	vars := map[string]string{
		"subject": fmt.Sprintf("You were given access to the workspace “%s” of “%s”", workspace, tenant),
		"message": fmt.Sprintf("An administrator of “%s” gave you the %s role in its workspace “%s”. Sign in to open it.",
			tenant, role, workspace),
		"tenant":      tenant,
		"workspace":   workspace,
		"role":        role,
		"customer_id": customerID,
	}
	if _, err := notification.NewStore(h.db.For(ctx)).Notify(ctx, userID, accessGrantedTemplate, vars, "workspace", workspaceID); err != nil {
		h.log.Warn().Err(err).Str("user_id", userID).Msg("access-granted notification not written")
	}
}

// oneLineName is a name an administrator chose, fit to quote in a message:
// control characters (line breaks above all) become spaces, and it is cut to
// 80 characters.
func oneLineName(s string) string {
	s = strings.Join(strings.FieldsFunc(s, unicode.IsControl), " ")
	if r := []rune(strings.TrimSpace(s)); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return strings.TrimSpace(s)
}
