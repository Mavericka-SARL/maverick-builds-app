package main

import (
	"fmt"
	"strings"
)

// bootstrap creates a fully independent tenant/app/model/developer/revision
// (mirrors cmd/seed-regional-planning's bootstrap, condensed) so this QA
// script never depends on any other demo existing or not existing.
func bootstrap(root *api) *api {
	// Idempotent delete-before-create: this tool never cleaned up its own
	// "Engine QA Co" tenant on any prior run, so every invocation left a
	// fresh one behind — 4 accumulated in the shared dev DB, discovered
	// live 2026-08-10 (same root cause, and same fix, as
	// cmd/verify-topology's own "verify-topology" customer earlier the
	// same day). Delete-before-create here guarantees at most one exists
	// at a time regardless of how any PREVIOUS run exited, including a
	// log.Fatalf mid-run (which — like cmd/verify-topology's fatalf —
	// terminates immediately with no deferred cleanup).
	for _, t := range root.as("platform_admin").callList("GET", "/api/admin/tenants", nil) {
		if name, _ := t["name"].(string); name == "Engine QA Co" {
			step("deleting stale tenant \"Engine QA Co\" from a prior run (%s)", id(t))
			root.as("platform_admin").call("DELETE", "/api/admin/tenants/"+id(t), nil)
		}
	}

	step("creating tenant \"Engine QA Co\"")
	tenant := root.as("platform_admin").call("POST", "/api/admin/tenants", map[string]any{
		"name": "Engine QA Co",
		"plan": "enterprise",
	})
	customerID := id(tenant)
	workspaceID, _ := tenant["workspace_id"].(string)
	if workspaceID == "" {
		panic("tenant creation did not return a workspace_id")
	}

	step("creating application \"Engine QA\"")
	app := root.as("platform_admin").call("POST", "/api/admin/applications", map[string]any{
		"customer_id": customerID,
		"name":        "Engine QA",
		"mode":        "planning",
	})
	appID := id(app)

	step("creating model")
	model := root.as("platform_admin").call("POST", "/api/admin/models", map[string]any{
		"application_id": appID,
		"name":           "Engine QA",
	})
	modelID := id(model)

	const email = "qa.tester@engine-qa.test"
	step("creating developer user %s", email)
	user := root.as("platform_admin").call("POST", "/api/admin/users", map[string]any{
		"email":        email,
		"display_name": "QA Tester",
	})
	userID := id(user)

	step("granting role \"developer\" scoped to the new workspace")
	root.as("platform_admin").call("POST", "/api/admin/users/"+userID+"/roles", map[string]any{
		"role":         "developer",
		"workspace_id": workspaceID,
	})

	dev := root.as("admin-created-" + email).withApp(appID).withModel(modelID)

	step("creating + activating revision \"Working\"")
	rev := dev.call("POST", "/api/developer/revisions", map[string]any{"name": "Working"})
	revID := id(rev)
	dev.call("PUT", fmt.Sprintf("/api/developer/revisions/%s/activate", revID), nil)

	return dev.withRevision(revID)
}

func createDimension(dev *api, name, parentDimensionID string) string {
	body := map[string]any{"name": name, "agg_rule": "sum", "revision_id": dev.revisionID}
	if parentDimensionID != "" {
		body["parent_dimension_id"] = parentDimensionID
	}
	return id(dev.call("POST", "/api/developer/dimensions", body))
}

func createMember(dev *api, dimensionID, code, label, parentMemberID string) string {
	body := map[string]any{"code": code, "label": label}
	if parentMemberID != "" {
		body["parent_member_id"] = parentMemberID
	}
	return id(dev.call("POST", fmt.Sprintf("/api/developer/dimensions/%s/members", dimensionID), body))
}

func createGrid(dev *api, name string) string {
	return id(dev.call("POST", "/api/developer/grids", map[string]any{
		"name": name, "revision_id": dev.revisionID,
	}))
}

func assignDimension(dev *api, gridID, dimensionID string) {
	dev.call("POST", fmt.Sprintf("/api/developer/grids/%s/dimensions/%s", gridID, dimensionID), nil)
}

func assignMetric(dev *api, gridID, metricID string) {
	dev.call("POST", fmt.Sprintf("/api/developer/grids/%s/metrics/%s", gridID, metricID), nil)
}

func createInputMetric(dev *api, name string) string {
	return id(dev.call("POST", "/api/developer/metrics", map[string]any{
		"name": name, "is_input": true, "revision_id": dev.revisionID,
	}))
}

func createCalcMetric(dev *api, name, formula, aggRule string) string {
	if !strings.HasPrefix(formula, "=") {
		formula = "=" + formula
	}
	body := map[string]any{
		"name": name, "is_input": false, "formula": formula, "revision_id": dev.revisionID,
	}
	if aggRule != "" {
		body["agg_rule"] = aggRule
	}
	return id(dev.call("POST", "/api/developer/metrics", body))
}

func writeCell(dev *api, metricID string, dimCodes map[string]string, value float64) {
	dev.call("POST", "/api/cells", map[string]any{
		"model_id":    dev.modelID,
		"metric_id":   metricID,
		"revision_id": dev.revisionID,
		"dim_codes":   dimCodes,
		"value":       value,
	})
}
