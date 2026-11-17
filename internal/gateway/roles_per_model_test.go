package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow/assignee"
)

// Business roles belong to the workspace, so every model of its
// applications shares them. The Roles page lists a model the roles it uses
// (used_here) and tells it which other models use each one (other_models);
// a rename or delete of a role other models use is refused (409
// role_used_elsewhere) unless confirm_other_models=true says the person
// agreed to change those models too.
func TestBusinessRolesListedPerModel(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()
	admin := baAdmin(t, f, "wt-roles-per-model")

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}

	// A second application of the same workspace, with its own model. Its
	// "Accounts" dimension may be edited by Viewers, its records table
	// created in by Clerks, and Auditors hold an access rule on one of its
	// members. Spare is used by no model.
	crmApp := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'CRM App', 'planning') RETURNING id::text`, f.wsID, f.custID)
	crmModel := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'CRM') RETURNING id::text`, crmApp)
	crmRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, crmModel)
	q(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid RETURNING id::text`, crmRev, crmModel)
	accounts := q(`INSERT INTO model.dimension_def (model_id, name, editor_roles) VALUES ($1::uuid, 'Accounts', '{Viewers}') RETURNING id::text`, crmModel)
	acme := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'ACME', 'Acme') RETURNING id::text`, accounts)
	q(`INSERT INTO model.form_def (model_id, name, label, create_roles) VALUES ($1::uuid, 'deals', 'Deals', '["Clerks"]') RETURNING id::text`, crmModel)
	clerks := q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Clerks') RETURNING id::text`, f.wsID)
	auditors := q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Auditors') RETURNING id::text`, f.wsID)
	spare := q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Spare') RETURNING id::text`, f.wsID)
	q(`INSERT INTO identity.business_role_access_rule (role_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'read') RETURNING id::text`, auditors, acme)
	// Night Shift: no definition names it any more, but a run of M's
	// workflow that started before the edit still waits on it.
	nightShift := q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Night Shift') RETURNING id::text`, f.wsID)
	q(`INSERT INTO workflow.workflow_instance (workflow_def_id, started_by, status, steps_snapshot)
	   VALUES ($1::uuid, $2::uuid, 'running', '[{"id":"s","name":"Check","type":"approval","assignee_roles":["Night Shift"]}]') RETURNING id::text`,
		f.wfDefID, f.ownerID)

	do := func(method, path, appID string, body any) (int, []byte) {
		t.Helper()
		var rd io.Reader = http.NoBody
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, f.srv.URL+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Dev-User", admin)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	list := func(appID string) map[string]baRoleRow {
		t.Helper()
		status, body := do("GET", "/api/business-admin/roles", appID, nil)
		if status != http.StatusOK {
			t.Fatalf("list roles: %d %s", status, body)
		}
		var rows []baRoleRow
		if err := json.Unmarshal(body, &rows); err != nil {
			t.Fatalf("decode roles: %v (%s)", err, body)
		}
		out := map[string]baRoleRow{}
		for _, r := range rows {
			out[r.ID] = r
		}
		return out
	}
	others := func(r baRoleRow) []string {
		var names []string
		for _, m := range r.OtherModels {
			names = append(names, m.ModelName)
		}
		return names
	}
	expect := func(where string, rows map[string]baRoleRow, roleID, name string, usedHere bool, other ...string) {
		t.Helper()
		r, ok := rows[roleID]
		if !ok {
			t.Fatalf("%s: role %s missing from the list", where, name)
		}
		got := others(r)
		if r.UsedHere != usedHere || len(got) != len(other) {
			t.Fatalf("%s: %s used_here=%v other_models=%v, want %v %v", where, name, r.UsedHere, got, usedHere, other)
		}
		for i := range other {
			if got[i] != other[i] {
				t.Fatalf("%s: %s other_models=%v, want %v", where, name, got, other)
			}
		}
	}

	// Budget Owners: a dashboard of M and its workflow; Budget Approvers:
	// M's workflow only (no revision on the definition: every model of its
	// application).
	inM := list(f.appID)
	expect("from M", inM, f.ownersRoleID, "Budget Owners", true)
	expect("from M", inM, f.approversRoleID, "Budget Approvers", true)
	expect("from M", inM, f.viewersRoleID, "Viewers", false, "CRM")
	expect("from M", inM, clerks, "Clerks", false, "CRM")
	expect("from M", inM, auditors, "Auditors", false, "CRM")
	expect("from M", inM, spare, "Spare", false)
	expect("from M", inM, nightShift, "Night Shift", true)

	inCRM := list(crmApp)
	expect("from CRM", inCRM, f.ownersRoleID, "Budget Owners", false, "M")
	expect("from CRM", inCRM, f.viewersRoleID, "Viewers", true)
	expect("from CRM", inCRM, clerks, "Clerks", true)
	expect("from CRM", inCRM, auditors, "Auditors", true)
	expect("from CRM", inCRM, spare, "Spare", false)
	expect("from CRM", inCRM, nightShift, "Night Shift", false, "M")

	name := func(id string) string {
		return q(`SELECT name FROM identity.business_role WHERE id=$1::uuid`, id)
	}

	// Renaming M's role from CRM is refused until confirmed.
	status, body := do("PATCH", "/api/business-admin/roles/"+f.ownersRoleID, crmApp, map[string]string{"name": "Budget Holders"})
	if status != http.StatusConflict || !bytes.Contains(body, []byte(`"role_used_elsewhere"`)) {
		t.Fatalf("unconfirmed rename of another model's role: %d %s, want 409 role_used_elsewhere", status, body)
	}
	if got := name(f.ownersRoleID); got != "Budget Owners" {
		t.Fatalf("refused rename still renamed the role to %q", got)
	}
	if status, body := do("PATCH", "/api/business-admin/roles/"+f.ownersRoleID+"?confirm_other_models=true", crmApp, map[string]string{"name": "Budget Holders"}); status != http.StatusOK {
		t.Fatalf("confirmed rename: %d %s", status, body)
	}
	if got := name(f.ownersRoleID); got != "Budget Holders" {
		t.Fatalf("confirmed rename left the name %q", got)
	}

	// A role only the open model uses, or no model, changes without asking.
	if status, body := do("PATCH", "/api/business-admin/roles/"+f.approversRoleID, f.appID, map[string]string{"name": "Budget Checkers"}); status != http.StatusOK {
		t.Fatalf("rename of the model's own role: %d %s", status, body)
	}
	if status, body := do("DELETE", "/api/business-admin/roles/"+spare, f.appID, nil); status != http.StatusOK {
		t.Fatalf("delete of an unused role: %d %s", status, body)
	}

	// Deleting CRM's role from M is refused until confirmed.
	if status, body := do("DELETE", "/api/business-admin/roles/"+clerks, f.appID, nil); status != http.StatusConflict {
		t.Fatalf("unconfirmed delete of another model's role: %d %s, want 409", status, body)
	}
	if status, body := do("DELETE", "/api/business-admin/roles/"+clerks+"?confirm_other_models=true", f.appID, nil); status != http.StatusOK {
		t.Fatalf("confirmed delete: %d %s", status, body)
	}
	if n := q(`SELECT count(*)::text FROM identity.business_role WHERE id=$1::uuid`, clerks); n != "0" {
		t.Fatalf("confirmed delete left the role")
	}
}

// A rename carries the new name into everything that names the role by name,
// in the same transaction: workflow definitions (step assignees, a
// notification's recipient, who may start and reopen), runs' steps
// snapshots, tables of records and dimension editors — and into a
// tenant-level application's workflows unless another workspace of the
// tenant still holds a role of the old name, which those names then mean.
func TestBusinessRoleRenameCarriesIntoWhatNamesIt(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()
	admin := baAdmin(t, f, "wt-role-rename")

	q := func(sql string, args ...any) string {
		t.Helper()
		var v string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return v
	}
	q(`UPDATE workflow.workflow_def SET start_roles='["Budget Owners"]', reopen_roles='["Budget Approvers","Viewers"]' WHERE id=$1::uuid RETURNING id::text`, f.wfDefID)
	run := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, started_by, status, steps_snapshot)
	          SELECT id, $2::uuid, 'running', steps FROM workflow.workflow_def WHERE id=$1::uuid RETURNING id::text`, f.wfDefID, f.ownerID)
	dim := q(`INSERT INTO model.dimension_def (model_id, name, editor_roles) VALUES ($1::uuid, 'Cost centres', '{Budget Owners,Viewers}') RETURNING id::text`, f.modelID)
	table := q(`INSERT INTO model.form_def (model_id, name, label, create_roles) VALUES ($1::uuid, 'requests', 'Requests', '["Budget Owners"]') RETURNING id::text`, f.modelID)
	// A tenant-level application (no workspace) whose steps name roles of
	// any workspace of the tenant; a second workspace holds its own Viewers.
	tenantApp := q(`INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'Tenant App', 'planning') RETURNING id::text`, f.custID)
	tenantWF := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status)
	               VALUES ($1::uuid, 'Tenant Flow', 'manual', '[{"id":"a","name":"A","type":"task","assignee_roles":["Budget Owners","Viewers"]}]', 'draft') RETURNING id::text`, tenantApp)
	ws2 := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws2') RETURNING id::text`, f.custID)
	q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Viewers') RETURNING id::text`, ws2)

	rename := func(roleID, name string) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"name": name})
		req, _ := http.NewRequestWithContext(ctx, "PATCH", f.srv.URL+"/api/business-admin/roles/"+roleID, bytes.NewReader(b))
		req.Header.Set("X-Dev-User", admin)
		req.Header.Set("X-App-Id", f.appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("rename to %s: %d %s", name, resp.StatusCode, raw)
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	expect := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %s, want %s", what, got, want)
		}
	}
	defSteps := func(id string) string {
		return q(`SELECT steps::text FROM workflow.workflow_def WHERE id=$1::uuid`, id)
	}

	res := rename(f.ownersRoleID, "Budget Holders")
	if got, _ := json.Marshal(res["renamed_in"]); !bytes.Contains(got, []byte(`"Budget Approval"`)) || !bytes.Contains(got, []byte(`"runs":1`)) {
		t.Errorf("renamed_in = %s, want the workflow and the run", got)
	}
	expect("step assignees", q(`SELECT string_agg(COALESCE(s->>'assignee_roles', '-'), ' | ' ORDER BY o) FROM workflow.workflow_def wd, jsonb_array_elements(wd.steps) WITH ORDINALITY x(s, o) WHERE wd.id=$1::uuid`, f.wfDefID),
		`["Budget Holders"] | ["Budget Approvers"] | -`)
	expect("notification recipient", q(`SELECT steps->2->'notification'->>'recipient_role' FROM workflow.workflow_def WHERE id=$1::uuid`, f.wfDefID), "Budget Holders")
	expect("notification subject kept", q(`SELECT steps->2->'notification'->>'subject' FROM workflow.workflow_def WHERE id=$1::uuid`, f.wfDefID), "Budget approved")
	expect("routes kept", q(`SELECT steps->1->'routes'->>'approve' FROM workflow.workflow_def WHERE id=$1::uuid`, f.wfDefID), "step-notify")
	expect("start roles", q(`SELECT start_roles::text FROM workflow.workflow_def WHERE id=$1::uuid`, f.wfDefID), `["Budget Holders"]`)
	expect("run snapshot", q(`SELECT steps_snapshot->0->>'assignee_roles' FROM workflow.workflow_instance WHERE id=$1::uuid`, run), `["Budget Holders"]`)
	expect("dimension editors", q(`SELECT array_to_string(editor_roles, ',') FROM model.dimension_def WHERE id=$1::uuid`, dim), "Budget Holders,Viewers")
	expect("table create roles", q(`SELECT create_roles::text FROM model.form_def WHERE id=$1::uuid`, table), `["Budget Holders"]`)
	// No other workspace has "Budget Owners": the tenant-level app follows.
	expect("tenant-level app", q(`SELECT steps->0->>'assignee_roles' FROM workflow.workflow_def WHERE id=$1::uuid`, tenantWF), `["Budget Holders", "Viewers"]`)

	// ws2 still has a Viewers: the tenant-level app's "Viewers" now means
	// that one and is left alone; this workspace's own names follow.
	rename(f.viewersRoleID, "Readers")
	expect("reopen roles", q(`SELECT reopen_roles::text FROM workflow.workflow_def WHERE id=$1::uuid`, f.wfDefID), `["Budget Approvers", "Readers"]`)
	expect("dimension editors", q(`SELECT array_to_string(editor_roles, ',') FROM model.dimension_def WHERE id=$1::uuid`, dim), "Budget Holders,Readers")
	expect("shadowed tenant-level app", q(`SELECT steps->0->>'assignee_roles' FROM workflow.workflow_def WHERE id=$1::uuid`, tenantWF), `["Budget Holders", "Viewers"]`)

	// The renamed role still reaches its member: Olga's task in the run.
	if !strings.Contains(defSteps(f.wfDefID), "Budget Holders") {
		t.Fatal("definition lost the new name")
	}
	var reaches bool
	if err := f.pool.QueryRow(ctx, `SELECT `+assignee.SQL("$1::uuid", `'["Budget Holders"]'::jsonb`, "$2::uuid"), f.appID, f.ownerID).Scan(&reaches); err != nil || !reaches {
		t.Fatalf("Budget Holders reaches its member: %v %v", reaches, err)
	}
}
