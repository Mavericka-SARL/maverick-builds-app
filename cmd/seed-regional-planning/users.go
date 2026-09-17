package main

type usersResult struct {
	alexID, priyaID, diegoID string
	financeReviewRoleID      string
}

// buildUsersAndAccess creates 3 business users and demonstrates all three
// dimension-member access levels (write/read/hidden) on a single dimension
// (department) across two users with mirrored, opposite scopes — so the
// mechanism is visibly general, not a one-off:
//
//   - Alex Chen (business_admin, member of the new "Finance Review" named
//     role) — no rules, so full default access everywhere.
//   - Priya Patel (business_user) — write on her own NA_* departments,
//     read (visible, not editable) on EU_*, hidden (no access at all) on
//     APAC_*.
//   - Diego Ramirez (business_user) — the mirror image for EU_*/NA_*, same
//     hidden treatment of APAC_*.
func buildUsersAndAccess(admin *api, boot bootstrapResult, d dims) usersResult {
	var u usersResult

	step("creating business_admin user Alex Chen")
	u.alexID = createUser(admin, "alex.chen@meridian-industries.demo", "Alex Chen", "business_admin", boot.workspaceID)

	step("creating business_user Priya Patel")
	u.priyaID = createUser(admin, "priya.patel@meridian-industries.demo", "Priya Patel", "business_user", boot.workspaceID)

	step("creating business_user Diego Ramirez")
	u.diegoID = createUser(admin, "diego.ramirez@meridian-industries.demo", "Diego Ramirez", "business_user", boot.workspaceID)

	// business_role management (POST /api/business-admin/roles[/{id}/members])
	// admits business_admin OR developer (the baOrDev guard widened earlier
	// this session so a developer can create the named roles their own
	// workflow steps reference) — done here as Alex, the business_admin,
	// which is the more realistic path for "who sets up named teams."
	alex := admin.as("admin-created-alex.chen@meridian-industries.demo").withApp(boot.appID)

	step("creating named business role \"Finance Review\"")
	role := alex.call("POST", "/api/business-admin/roles", map[string]any{"name": "Finance Review"})
	u.financeReviewRoleID = id(role)
	alex.call("POST", "/api/business-admin/roles/"+u.financeReviewRoleID+"/members", map[string]any{"user_id": u.alexID})
	// Dashboard access is granted later, by grantFinanceReviewDashboardAccess
	// in main.go — no dashboard exists yet at this point in the seed
	// sequence (buildDashboard runs after this function). Without that
	// later grant, the "no rules" comment above is misleading: a
	// business_role_dashboard membership with zero grants doesn't mean
	// "unrestricted," it means "restricted to nothing" — see that
	// function's own comment for the live 403 this caused.

	step("setting Priya's department access rules (write NA_*, read EU_*, hidden APAC_*)")
	setAccessRules(alex, u.priyaID, []accessRule{
		{RefID: d.department["NA_SALES"], Access: "write"},
		{RefID: d.department["NA_ENG"], Access: "write"},
		{RefID: d.department["EU_SALES"], Access: "read"},
		{RefID: d.department["EU_ENG"], Access: "read"},
		{RefID: d.department["APAC_SALES"], Access: "hidden"},
	})

	step("setting Diego's department access rules (write EU_*, read NA_*, hidden APAC_*)")
	setAccessRules(alex, u.diegoID, []accessRule{
		{RefID: d.department["EU_SALES"], Access: "write"},
		{RefID: d.department["EU_ENG"], Access: "write"},
		{RefID: d.department["NA_SALES"], Access: "read"},
		{RefID: d.department["NA_ENG"], Access: "read"},
		{RefID: d.department["APAC_SALES"], Access: "hidden"},
	})

	return u
}

func createUser(admin *api, email, displayName, role, workspaceID string) string {
	user := admin.call("POST", "/api/admin/users", map[string]any{
		"email":        email,
		"display_name": displayName,
	})
	userID := id(user)
	admin.call("POST", "/api/admin/users/"+userID+"/roles", map[string]any{
		"role":         role,
		"workspace_id": workspaceID,
	})
	return userID
}

type accessRule struct {
	RefID  string `json:"ref_id"`
	Access string `json:"access"`
}

func setAccessRules(businessAdmin *api, userID string, rules []accessRule) {
	full := make([]map[string]string, 0, len(rules))
	for _, r := range rules {
		full = append(full, map[string]string{"rule_type": "dimension_member", "ref_id": r.RefID, "access": r.Access})
	}
	// PUT replaces the whole rule set for this user — always send every
	// rule the user should have, not just a delta.
	businessAdmin.call("PUT", "/api/business-admin/users/"+userID+"/access-rules", map[string]any{"rules": full})
}
