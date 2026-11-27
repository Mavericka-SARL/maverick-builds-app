package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A table naming who changes each record (migration 200): to a holder of
// one of its roles, the members whose record names someone else in the
// person field are read-only — their records and their cells — while every
// member stays visible; a person outside the roles keeps the access they
// had; clearing the setting gives today's behaviour back.
func TestPersonFieldNamesWhoChangesEachRecord(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	projDim := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "project", "revision_id": d.revID}))
	table := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "projects", "label": "Projects", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "code", "label": "Project ID", "type": "text", "code": true, "core": true},
			{"name": "name", "label": "Project", "type": "text", "required": true},
			{"name": "manager", "label": "Project manager", "type": "person"},
		},
		"keeps_dimension_id": projDim, "member_label_field": "name",
	}))
	p1 := idOf(d.call("POST", "/api/forms/"+table+"/records", d.dev, map[string]any{"data": map[string]any{"code": "P1", "name": "West clinic", "manager": d.westRepID}}))
	p2 := idOf(d.call("POST", "/api/forms/"+table+"/records", d.dev, map[string]any{"data": map[string]any{"code": "P2", "name": "East school", "manager": d.eastRepID}}))
	var roleID string
	if err := d.pool.QueryRow(ctx, `INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Project Manager') RETURNING id::text`, d.wsID).Scan(&roleID); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{d.westRepID, d.eastRepID} {
		if _, err := d.pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, roleID, u); err != nil {
			t.Fatal(err)
		}
	}
	// A cell input on the projects.
	grid := idOf(d.call("POST", "/api/developer/grids", d.dev, map[string]any{"name": "Project budget", "revision_id": d.revID}))
	d.call("POST", "/api/developer/grids/"+grid+"/dimensions/"+projDim, d.dev, nil)
	budget := idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{"name": "proj_budget", "label": "Budget", "is_input": true,
		"revision_id": d.revID, "format": "number", "agg_rule": "sum"}))
	d.call("POST", "/api/developer/grids/"+grid+"/metrics/"+budget, d.dev, nil)
	writeBudget := func(persona, code string, v float64) int {
		st, _ := d.req("POST", "/api/cells", persona, map[string]any{"model_id": d.modelID, "revision_id": d.revID, "metric_id": budget,
			"dim_codes": map[string]string{projDim: code}, "value": v})
		return st
	}
	rename := func(persona, rec, code, name, manager string) int {
		st, _ := d.req("PUT", "/api/records/"+rec, persona, map[string]any{"data": map[string]any{"code": code, "name": name, "manager": manager}})
		return st
	}

	// Refused settings: no person field, no roles, a role that is not there.
	for _, c := range []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"editor_field": "name", "editor_roles": []string{"Project Manager"}}, "is no person field of Projects"},
		{map[string]any{"editor_field": "manager", "editor_roles": []string{}}, "name the business roles"},
		{map[string]any{"editor_field": "manager", "editor_roles": []string{"Project Lead"}}, "no business role here"},
	} {
		if st, raw := d.req("PATCH", "/api/forms/"+table, d.dev, c.body); st != http.StatusBadRequest || !strings.Contains(string(raw), c.want) {
			t.Errorf("settings %v: %d %s, want 400 saying %q", c.body, st, raw, c.want)
		}
	}
	if st, raw := d.req("PATCH", "/api/forms/"+table, d.dev, map[string]any{"editor_field": "manager", "editor_roles": []string{"Project Manager"}}); st != http.StatusOK {
		t.Fatalf("set the editor field: %d %s", st, raw)
	}

	if st := rename(d.westRep, p1, "P1", "West clinic (phase 2)", d.westRepID); st != http.StatusOK {
		t.Errorf("the west rep renames the project naming them: %d, want 200", st)
	}
	if st := rename(d.westRep, p2, "P2", "East school (taken)", d.eastRepID); st != http.StatusForbidden {
		t.Errorf("the west rep renames the east rep's project: %d, want 403", st)
	}
	if st := writeBudget(d.westRep, "P1", 100); st != http.StatusOK {
		t.Errorf("the west rep budgets their project: %d, want 200", st)
	}
	if st := writeBudget(d.westRep, "P2", 100); st == http.StatusOK {
		t.Errorf("the west rep budgets the east rep's project: %d, want a refusal", st)
	}
	if st := writeBudget(d.eastRep, "P2", 50); st != http.StatusOK {
		t.Errorf("the east rep budgets their project: %d, want 200", st)
	}
	// Both projects stay visible to the west rep.
	g := d.call("GET", "/api/grid?revision_id="+d.revID+"&grid_def_id="+grid, d.westRep, nil)
	seen := map[string]bool{}
	for _, dim := range g["dimensions"].([]any) {
		for _, m := range dim.(map[string]any)["members"].([]any) {
			seen[m.(map[string]any)["code"].(string)] = true
		}
	}
	if !seen["P1"] || !seen["P2"] {
		t.Errorf("the west rep sees %v, want both projects", seen)
	}
	// Someone outside the role keeps the access they had.
	if st := rename(d.dev, p2, "P2", "East school", d.eastRepID); st != http.StatusOK {
		t.Errorf("the developer renames the east rep's project: %d, want 200", st)
	}
	// Handing the project over hands its editing over.
	if st := rename(d.dev, p2, "P2", "East school", d.westRepID); st != http.StatusOK {
		t.Fatalf("hand P2 to the west rep: %d", st)
	}
	if st := writeBudget(d.westRep, "P2", 70); st != http.StatusOK {
		t.Errorf("the west rep budgets the project handed to them: %d, want 200", st)
	}
	// In a tenant-level application (no workspace of its own) the role counts
	// from any workspace of its tenant, as a workflow step's roles do.
	if _, err := d.pool.Exec(ctx, `UPDATE core.application SET workspace_id = NULL WHERE id = $1::uuid`, d.appID); err != nil {
		t.Fatal(err)
	}
	if st := rename(d.eastRep, p1, "P1", "West clinic (east's now?)", d.westRepID); st != http.StatusForbidden {
		t.Errorf("tenant-level application: the east rep renames the west rep's project: %d, want 403", st)
	}
	// Cleared, everyone in the role edits every project again.
	if st, raw := d.req("PATCH", "/api/forms/"+table, d.dev, map[string]any{"editor_field": ""}); st != http.StatusOK {
		t.Fatalf("clear the editor field: %d %s", st, raw)
	}
	if st := rename(d.eastRep, p1, "P1", "West clinic", d.westRepID); st != http.StatusOK {
		t.Errorf("after clearing, the east rep renames the west rep's project: %d, want 200", st)
	}
}
