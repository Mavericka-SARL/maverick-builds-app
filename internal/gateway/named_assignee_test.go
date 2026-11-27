package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A step naming its person from the run's data (assignee_field, owner
// decision 2026-11-27): the Senegal plan's approval goes to Senegal's country
// manager only — in their inbox, decided by them, refused to the other
// country manager — while a country without a manager, or whose manager
// holds none of the step's roles, leaves the step to its role as before.
func TestStepGoesToThePersonTheRunNames(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	countryDim := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "country", "revision_id": d.revID}))
	projDim := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "project", "revision_id": d.revID}))
	countries := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "countries", "label": "Countries", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "code", "label": "Code", "type": "text", "code": true, "core": true},
			{"name": "name", "label": "Country", "type": "text", "required": true},
			{"name": "manager", "label": "Country manager", "type": "person"},
		},
		"keeps_dimension_id": countryDim, "member_label_field": "name",
	}))
	projects := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "projects", "label": "Projects", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "code", "label": "Project ID", "type": "text", "code": true, "core": true},
			{"name": "name", "label": "Project", "type": "text", "required": true},
			{"name": "country", "label": "Country", "type": "dimension", "dimension_id": countryDim},
		},
		"keeps_dimension_id": projDim, "member_label_field": "name",
	}))
	for _, c := range []map[string]any{
		{"code": "SN", "name": "Senegal", "manager": d.westRepID},
		{"code": "RW", "name": "Rwanda", "manager": d.roID}, // holds no Country Manager role
		{"code": "LA", "name": "Laos"},                      // no manager
	} {
		d.call("POST", "/api/forms/"+countries+"/records", d.dev, map[string]any{"data": c})
	}
	for _, p := range []map[string]any{
		{"code": "P1", "name": "Clinic", "country": "SN"},
		{"code": "P2", "name": "School", "country": "RW"},
		{"code": "P3", "name": "Bridge", "country": "LA"},
	} {
		d.call("POST", "/api/forms/"+projects+"/records", d.dev, map[string]any{"data": p})
	}
	var roleID string
	if err := d.pool.QueryRow(ctx, `INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Country Manager') RETURNING id::text`, d.wsID).Scan(&roleID); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{d.westRepID, d.eastRepID} {
		if _, err := d.pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, roleID, u); err != nil {
			t.Fatal(err)
		}
	}

	wf := idOf(d.call("POST", "/api/developer/workflows?application_id="+d.appID+"&revision_id="+d.revID, d.dev, map[string]any{"name": "Plan approval"}))
	steps := []map[string]any{
		{"id": "review", "name": "Review the plan", "type": "approval", "assignee_roles": []string{"Country Manager"},
			"assignee_field": "project.country.manager", "routes": map[string]string{"approve": "end-completed", "reject": "end-rejected"}},
	}
	schema := []map[string]any{{"key": "project", "data_type": "dimension_member", "required": true, "dimension_id": projDim}}
	d.call("PATCH", "/api/developer/workflows/"+wf, d.dev, map[string]any{"steps": steps, "context_schema": schema})
	if st, raw := d.req("POST", "/api/developer/workflows/"+wf+"/publish", d.dev, nil); st != http.StatusOK {
		t.Fatalf("publish: %d %s", st, raw)
	}
	start := func(project string) string {
		t.Helper()
		st, raw := d.req("POST", "/api/workflow/instances", d.dev, map[string]any{"workflow_def_id": wf, "context": map[string]string{"project": project}})
		if st != http.StatusOK && st != http.StatusCreated {
			t.Fatalf("start for %s: %d %s", project, st, raw)
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		id, _ := out["id"].(string)
		if id == "" {
			id, _ = out["instance_id"].(string)
		}
		return id
	}
	inbox := func(persona, instance string) string {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			st, raw := d.req("GET", "/api/tasks", persona, nil)
			if st != http.StatusOK {
				t.Fatalf("inbox of %s: %d %s", persona, st, raw)
			}
			var tasks []map[string]any
			_ = json.Unmarshal(raw, &tasks)
			for _, tk := range tasks {
				if tk["instance_id"] == instance {
					return tk["id"].(string)
				}
			}
			if time.Now().After(deadline) {
				return ""
			}
		}
	}

	sn := start("P1")
	if inbox(d.westRep, sn) == "" {
		t.Errorf("Senegal's plan is not in Senegal's country manager's inbox")
	}
	task := inbox(d.westRep, sn)
	if inbox(d.eastRep, sn) != "" {
		t.Errorf("Senegal's plan is in another country manager's inbox")
	}
	if st, _ := d.req("POST", "/api/tasks/"+task+"/complete", d.eastRep, map[string]any{"decision": "approve", "comment": "not mine"}); st == http.StatusOK {
		t.Errorf("another country manager decided Senegal's plan")
	}
	if st, raw := d.req("POST", "/api/tasks/"+task+"/complete", d.westRep, map[string]any{"decision": "approve", "comment": "ok"}); st != http.StatusOK {
		t.Errorf("Senegal's country manager decides Senegal's plan: %d %s", st, raw)
	}

	// Rwanda's manager holds no Country Manager role, Laos has none: both
	// plans go to every country manager, as without the field.
	for _, p := range []string{"P2", "P3"} {
		run := start(p)
		if inbox(d.westRep, run) == "" || inbox(d.eastRep, run) == "" {
			t.Errorf("%s's plan: not in every country manager's inbox", p)
		}
	}
}
