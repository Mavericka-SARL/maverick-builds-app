package main

import "fmt"

type bootstrapResult struct {
	customerID  string
	workspaceID string
	appID       string
	modelID     string
	revisionID  string
}

// bootstrap creates the one-time container (customer → application →
// model → revision) that only platform_admin/tenant_admin can create —
// confirmed during planning there is no developer-accessible endpoint for
// this, and the user confirmed that's correct role separation (tenant_admin
// provisions the container; the developer persona builds everything inside
// it, in every phase after this one).
func bootstrap(a *api) bootstrapResult {
	step("creating tenant \"Meridian Industries\"")
	tenant := a.call("POST", "/api/admin/tenants", map[string]any{
		"name": "Meridian Industries",
		"plan": "enterprise",
	})
	customerID := id(tenant)
	workspaceID, _ := tenant["workspace_id"].(string)
	if workspaceID == "" {
		panic("tenant creation did not return a workspace_id")
	}

	step("creating application \"Regional Expense Planning\"")
	app := a.call("POST", "/api/admin/applications", map[string]any{
		"customer_id": customerID,
		"name":        "Regional Expense Planning",
		"mode":        "planning",
	})
	appID := id(app)

	step("creating model")
	model := a.call("POST", "/api/admin/models", map[string]any{
		"application_id": appID,
		"name":           "Regional Expense Planning",
	})
	modelID := id(model)

	res := bootstrapResult{customerID: customerID, workspaceID: workspaceID, appID: appID, modelID: modelID}
	return res
}

// createAndActivateRevision must run as the developer persona (revisions
// are a developer-guarded resource) — called from provisionDeveloper, once
// the developer persona and X-App-Id scoping are both in place.
func createAndActivateRevision(dev *api) string {
	step("creating revision %q", revisionName)
	rev := dev.call("POST", "/api/developer/revisions", map[string]any{"name": revisionName})
	revID := id(rev)
	step("activating revision %q", revisionName)
	dev.call("PUT", fmt.Sprintf("/api/developer/revisions/%s/activate", revID), nil)
	return revID
}

// provisionDeveloper creates a developer user scoped into the new
// workspace (POST /api/admin/users + .../roles, both admitting a
// platform_admin/tenant_admin actor to assign the "developer" role per
// roleIsAssignableBy), then switches to that persona to create and
// activate the working revision — every remaining phase of this build
// runs as this developer, exactly as a real developer would experience it
// after being granted access to a new app.
func provisionDeveloper(admin *api, boot bootstrapResult) (*api, bootstrapResult) {
	const email = "sam.developer@meridian-industries.demo"
	step("creating developer user %s", email)
	user := admin.call("POST", "/api/admin/users", map[string]any{
		"email":        email,
		"display_name": "Sam Developer",
	})
	userID := id(user)

	step("granting role \"developer\" scoped to the new workspace")
	admin.call("POST", "/api/admin/users/"+userID+"/roles", map[string]any{
		"role":         "developer",
		"workspace_id": boot.workspaceID,
	})

	// "admin-created-<email>" is exactly the keycloak_sub POST /api/admin/users
	// generates (handler.go's adminUserAction) — resolveDevActor treats any
	// X-Dev-User value that isn't a predefined persona key as a keycloak_sub
	// directly, so this user is immediately usable, no separate step needed.
	dev := admin.as("admin-created-" + email).withApp(boot.appID).withModel(boot.modelID)

	boot.revisionID = createAndActivateRevision(dev)
	return dev.withRevision(boot.revisionID), boot
}
