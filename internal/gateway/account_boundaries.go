package gateway

// Account boundaries for the users screen (decided 2026-09-30).
//
// An account has a home: its identity.user customer_id. An administrator who
// is not a platform admin changes an account itself — renames, deletes,
// disables, re-invites it, changes its e-mail — only when that home is one of
// the tenants the administrator administers (adminScopeCustomerIDs), it is
// not a platform admin, and the administrator may revoke every role it holds.
// An account of another tenant, or of none, that
// holds roles in the administrator's workspaces is listed, and can be removed
// from the tenant: what it holds there goes, nothing else about it changes.
// Inviting an address that already has an account adds it to the tenant
// the same way, inside a workspace, and leaves the account as it is.
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

	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
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
	// caller's tenants.
	ScopeRoles []string
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

// homeTenant classifies customerID against the caller's admin scope
// (adminScopeCustomerIDs). A caller whose scope is every tenant counts every
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
// adminScopeCustomerIDs): its home is one of the caller's tenants, or — for a
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
// only one whose every role it may revoke (roleIsAssignableBy): deleting an
// account takes all of them, and renaming or re-inviting it is no less its
// administrator's business. A developer could delete its own tenant's
// administrator, identity-provider account and all, though it may not
// revoke that administrator's role (2026-09-30). Its own account it renames
// and re-invites whatever it holds. It removes from its tenant an account
// whose home is elsewhere — when it may manage resource access at all, and
// may revoke every role the account holds there (a tenant_admin grant a
// platform admin made stays).
func accountPermissionsFor(act *actor, scopeAll bool, scope []string, f accountFacts) (string, accountPermissions) {
	home := homeTenant(f.CustomerID, scopeAll, scope)
	if act.hasRole("platform_admin") {
		return home, accountPermissions{Rename: true, Delete: !f.Self, Disable: true, Reinvite: true}
	}
	if f.PlatformAdmin || f.PlatformWide {
		return home, accountPermissions{}
	}
	own := accountIsCallers(f.CustomerID, scopeAll, scope)
	change := own && (f.Self || revokesEvery(act, f.Roles))
	p := accountPermissions{Rename: change, Delete: change && !f.Self, Disable: change, Reinvite: change}
	p.RemoveFromTenant = !own && !scopeAll && !f.Self && canManageResourceAccess(act.Roles) && revokesEvery(act, f.ScopeRoles)
	return home, p
}

// revokesEvery reports whether act may revoke every one of roles.
func revokesEvery(act *actor, roles []string) bool {
	return !slices.ContainsFunc(roles, func(r string) bool { return !roleIsAssignableBy(act.Roles, r) })
}

// loadAccountFacts reads accountFacts for ids, keyed by the id as the
// database spells it. scope is the caller's adminScopeCustomerIDs.
func (h *handler) loadAccountFacts(ctx context.Context, act *actor, scope, ids []string) (map[string]accountFacts, error) {
	if scope == nil {
		scope = []string{}
	}
	rows, err := h.db.Query(ctx, `
		SELECT u.id::text, u.email, COALESCE(u.customer_id::text, ''),
		       EXISTS (SELECT 1 FROM identity.role_assignment pa WHERE pa.user_id = u.id AND pa.role = 'platform_admin'),
		       `+platformWideBuilderSQL("u.id")+`,
		       u.id = NULLIF($2, '')::uuid,
		       ARRAY(SELECT DISTINCT ra.role::text FROM identity.role_assignment ra
		             JOIN core.workspace w ON w.id = ra.workspace_id
		             WHERE ra.user_id = u.id AND w.customer_id::text = ANY($3::text[])
		             ORDER BY 1),
		       ARRAY(SELECT DISTINCT ra.role::text FROM identity.role_assignment ra WHERE ra.user_id = u.id ORDER BY 1)
		FROM identity."user" u
		WHERE u.id = ANY($1::uuid[])`, ids, act.UserID, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]accountFacts, len(ids))
	for rows.Next() {
		var f accountFacts
		if err := rows.Scan(&f.ID, &f.Email, &f.CustomerID, &f.PlatformAdmin, &f.PlatformWide, &f.Self, &f.ScopeRoles, &f.Roles); err != nil {
			return nil, err
		}
		out[f.ID] = f
	}
	return out, rows.Err()
}

// accountFactsOf is loadAccountFacts for one account, spelled any way a uuid
// can be. found is false when there is no such account.
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
func (h *handler) accountActionAllowed(ctx context.Context, w http.ResponseWriter, act *actor, scopeAll bool, scope []string,
	userID, action string, allowed func(accountPermissions) bool) bool {
	f, found, err := h.accountFactsOf(ctx, act, scope, userID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return false
	}
	if !found {
		jsonErr(w, fmt.Errorf("user not found"), http.StatusNotFound)
		return false
	}
	home, p := accountPermissionsFor(act, scopeAll, scope, f)
	if allowed(p) {
		return true
	}
	if accountIsCallers(f.CustomerID, scopeAll, scope) && !f.PlatformAdmin && !f.PlatformWide {
		unrevocable := slices.DeleteFunc(slices.Clone(f.Roles), func(r string) bool { return roleIsAssignableBy(act.Roles, r) })
		jsonErr(w, fmt.Errorf("forbidden: this account holds a role you may not revoke (%s), so you may not %s it either",
			strings.Join(unrevocable, ", "), action), http.StatusForbidden)
		return false
	}
	jsonErr(w, accountActionRefusal(home, action), http.StatusForbidden)
	return false
}

// withUserPermissions fills each listed user's home_tenant and permissions
// for the caller.
func (h *handler) withUserPermissions(ctx context.Context, act *actor, scopeAll bool, scope []string, users []adminUserItem) error {
	if len(users) == 0 {
		return nil
	}
	ids := make([]string, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	facts, err := h.loadAccountFacts(ctx, act, scope, ids)
	if err != nil {
		return err
	}
	for i := range users {
		f, ok := facts[users[i].ID]
		if !ok {
			// Deleted between the two reads: nothing may be done to it.
			users[i].HomeTenant = homeNone
			continue
		}
		users[i].HomeTenant, users[i].Permissions = accountPermissionsFor(act, scopeAll, scope, f)
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
// one that makes such an account platform-wide, and so an administrator of
// every tenant (adminScopeCustomerIDs); once it is gone the account builds
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
// the caller's tenants (removeTenantAccess), in one transaction that locks
// the account. If that was the last application or model grant narrowing a
// developer with no tenant, its unscoped developer grant goes too
// (revokeUnnarrowed). Refused as accountPermissionsFor says.
func (h *handler) adminRemoveFromTenant(ctx context.Context, w http.ResponseWriter, act *actor, scopeAll bool, scope []string, userID string) {
	f, found, err := h.accountFactsOf(ctx, act, scope, userID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !found {
		jsonErr(w, fmt.Errorf("user not found"), http.StatusNotFound)
		return
	}
	home, p := accountPermissionsFor(act, scopeAll, scope, f)
	if !p.RemoveFromTenant {
		switch {
		case !canManageResourceAccess(act.Roles):
			jsonErr(w, fmt.Errorf("forbidden: only a tenant admin can remove an account from the tenant"), http.StatusForbidden)
		case scopeAll:
			jsonErr(w, fmt.Errorf("your scope is every tenant, so there is no one tenant to remove this account from: "+
				"revoke its roles and grants one by one, or delete the account"), http.StatusBadRequest)
		case f.Self:
			jsonErr(w, fmt.Errorf("you cannot remove yourself from your tenant; ask another administrator"), http.StatusForbidden)
		case home == homeOwn:
			jsonErr(w, fmt.Errorf("this account belongs to your tenant: delete it, or revoke its roles one by one"), http.StatusBadRequest)
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
	unrevocable = slices.DeleteFunc(unrevocable, func(r string) bool { return roleIsAssignableBy(act.Roles, r) })
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
// sub, preferring the e-mail's, or "".
func (h *handler) existingAccountID(ctx context.Context, email, sub string) (string, error) {
	var id string
	err := h.db.QueryRow(ctx, `
		SELECT id::text FROM identity.user
		WHERE ($1 <> '' AND lower(email) = lower($1)) OR ($2 <> '' AND keycloak_sub = $2)
		ORDER BY ($1 <> '' AND lower(email) = lower($1)) DESC
		LIMIT 1`, email, sub).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
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
// for the platform only (refuseExistingAccount): a request with no role or
// no workspace, which a new address would have been created for; a platform
// admin's account or a platform-wide builder's, which only a platform admin
// changes; and an account with no tenant holding tenant_admin without a
// workspace, which any role in a new tenant's workspace would make that
// tenant's administrator (customerlessAdminGainsTenant). A 400 naming the
// existing account, and the real id of a platform admin's, used to tell the
// caller the address was taken, and whose it was (2026-09-30).
func (h *handler) addExistingAccount(ctx context.Context, w http.ResponseWriter, act *actor, userID, email, role, workspaceID string) {
	if role == "" || workspaceID == "" {
		h.refuseExistingAccount(ctx, w, act, userID, email, role, workspaceID,
			"an existing account is added to a tenant only with a role inside a workspace")
		return
	}
	var platformAdmin, platformWide bool
	if err := h.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM identity.role_assignment WHERE user_id = $1::uuid AND role = 'platform_admin'),
		       `+platformWideBuilderSQL("$1::uuid"), userID).Scan(&platformAdmin, &platformWide); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if platformAdmin || platformWide {
		h.refuseExistingAccount(ctx, w, act, userID, email, role, workspaceID,
			"a platform admin or platform-wide builder is modified only by a platform admin")
		return
	}
	if !roleIsAssignableBy(act.Roles, "tenant_admin") {
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
	jsonOK(w, map[string]any{"id": userID, "status": "created", "invited": h.kc != nil})
}

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
// (adminScopeCustomerIDs): even business_user in a new tenant's workspace is
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
