package main

// existingDemoNames are every application created by the other cmd/seed-*
// programs in this repo (all of which write directly to Postgres — see the
// package doc comment). Matched by exact name rather than "delete
// everything" so this stays a targeted, intentional cleanup.
var existingDemoNames = map[string]bool{
	"OPEX Planning 2026":        true,
	"Budget Planning FY2026":    true,
	"Sales Tracker":             true,
	"Budgeting Demo":            true,
	"CapEx Portfolio FY2026":    true,
	"OtherCorp HQ":              true,
	"Procurement Planning 2026": true,
}

// deleteExistingDemos removes every known demo application. Cascades
// (ON DELETE CASCADE, verified against every relevant migration) take the
// model, revisions, dimensions/members, metrics, grids, forms/records,
// workflows, dashboards, and integrations with it. It intentionally does
// NOT touch identity.user, core.workspace, or core.customer rows — there is
// no HTTP path to delete those (confirmed during planning), so they're left
// behind as harmless orphans rather than reached around via direct SQL.
func deleteExistingDemos(a *api) {
	apps := a.callList("GET", "/api/admin/applications", nil)
	deleted := 0
	for _, app := range apps {
		name, _ := app["name"].(string)
		if !existingDemoNames[name] {
			continue
		}
		appID := id(app)
		step("deleting application %q (%s)", name, appID)
		a.call("DELETE", "/api/admin/applications/"+appID, nil)
		deleted++
	}
	if deleted == 0 {
		step("no matching demo applications found — nothing to delete")
	} else {
		step("deleted %d demo application(s)", deleted)
	}
}
