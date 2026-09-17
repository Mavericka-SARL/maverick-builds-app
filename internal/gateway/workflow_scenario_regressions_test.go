package gateway

// Regressions from the 2026-09-13 workflow scenario run (a small model driven
// end to end as developer, business admin and business users):
//   - a developer's TEST RUN put its human step into real inboxes and, being
//     "running" with the same context, blocked every real start with 409;
//   - a required Dimension-member context variable was not enforced and an
//     unknown member code started an instance;
//   - editing an archived definition, or creating one named like an archived
//     definition, returned 500;
//   - the developer's instance list hardcoded status "running";
//   - the legacy /api/workflow/submit picked the newest definition regardless
//     of status and hand-inserted steps without the engine.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestWorkflowScenarioRegressions(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	devSub := "wt-dev"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev@wt.com', 'Dev', $2::uuid) RETURNING id::text`, devSub, f.custID)
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, devID, f.wsID); err != nil {
		t.Fatalf("developer role: %v", err)
	}
	dimID := q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'dept', $2::uuid) RETURNING id::text`, f.modelID, f.revID)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'SALES', 'Sales') RETURNING id::text`, dimID)

	devPath := "/api/developer/workflows?application_id=" + f.appID + "&revision_id=" + f.revID
	createDef := func(name string) (int, map[string]any) {
		status, body := f.do(t, "POST", devPath, devSub, map[string]any{"name": name, "trigger_event": "manual"})
		var res map[string]any
		_ = json.Unmarshal(body, &res)
		return status, res
	}
	status, res := createDef("Scenario")
	if status != http.StatusOK {
		t.Fatalf("create def: %d %v", status, res)
	}
	defID, _ := res["id"].(string)
	steps := []map[string]any{{"id": "t1", "name": "Review", "type": "task", "assignee_roles": []string{"Budget Owners"}, "routes": map[string]string{"next": "end-completed"}}}
	ctxSchema := []map[string]any{{"key": "dept", "label": "Department", "data_type": "Dimension member", "required": true, "dimension_id": dimID}}
	if status, body := f.do(t, "PATCH", "/api/developer/workflows/"+defID, devSub, map[string]any{"steps": steps, "context_schema": ctxSchema}); status != http.StatusOK {
		t.Fatalf("patch def: %d %s", status, body)
	}
	if status, body := f.do(t, "POST", "/api/developer/workflows/"+defID+"/publish", devSub, nil); status != http.StatusOK {
		t.Fatalf("publish: %d %s", status, body)
	}
	scenarioTasks := func(sub string) []map[string]any {
		_, body := f.do(t, "GET", "/api/tasks", sub, nil)
		var all []map[string]any
		_ = json.Unmarshal(body, &all)
		var out []map[string]any
		for _, task := range all {
			if task["workflow_name"] == "Scenario" {
				out = append(out, task)
			}
		}
		return out
	}

	t.Run("test run is invisible to inboxes and never blocks a real start", func(t *testing.T) {
		if status, body := f.do(t, "POST", "/api/developer/workflows/"+defID+"/test-run", devSub, map[string]any{"context": map[string]string{"dept": "SALES"}}); status != http.StatusOK {
			t.Fatalf("test-run: %d %s", status, body)
		}
		if got := scenarioTasks(f.ownerSub); len(got) != 0 {
			t.Errorf("the test run's task landed in a real inbox: %v", got)
		}
		status, body := f.do(t, "POST", "/api/workflow/instances", f.ownerSub, map[string]any{"workflow_def_id": defID, "context": map[string]string{"dept": "SALES"}})
		if status != http.StatusOK {
			t.Fatalf("real start after a test run with the same context: %d %s (want 200 — the dry run must not count as a running duplicate)", status, body)
		}
		if got := scenarioTasks(f.ownerSub); len(got) != 1 {
			t.Errorf("real instance's task in the owner's inbox: %d tasks, want 1", len(got))
		}
	})

	t.Run("missing required and unknown dimension members are 400", func(t *testing.T) {
		if status, body := f.do(t, "POST", "/api/workflow/instances", f.ownerSub, map[string]any{"workflow_def_id": defID, "context": map[string]string{}}); status != http.StatusBadRequest || !strings.Contains(string(body), "required") {
			t.Errorf("missing required dept: %d %s (want 400 naming the missing value)", status, body)
		}
		if status, body := f.do(t, "POST", "/api/workflow/instances", f.ownerSub, map[string]any{"workflow_def_id": defID, "context": map[string]string{"dept": "NOPE"}}); status != http.StatusBadRequest || !strings.Contains(string(body), "does not exist") {
			t.Errorf("unknown member: %d %s (want 400)", status, body)
		}
	})

	t.Run("archived definitions refuse edits and keep their name with a 409", func(t *testing.T) {
		status, res := createDef("Archive me")
		if status != http.StatusOK {
			t.Fatalf("create: %d %v", status, res)
		}
		id, _ := res["id"].(string)
		if status, body := f.do(t, "POST", "/api/developer/workflows/"+id+"/archive", devSub, nil); status != http.StatusOK {
			t.Fatalf("archive: %d %s", status, body)
		}
		if status, body := f.do(t, "PATCH", "/api/developer/workflows/"+id, devSub, map[string]any{"description": "x"}); status != http.StatusConflict {
			t.Errorf("edit archived: %d %s (want 409)", status, body)
		}
		if status, res := createDef("Archive me"); status != http.StatusConflict {
			t.Errorf("create with an archived name: %d %v (want 409, was a raw 500 duplicate-key)", status, res)
		}
	})

	t.Run("instances list reports the real status, context and test-run flag", func(t *testing.T) {
		_, body := f.do(t, "GET", "/api/developer/workflows/"+defID+"/instances", devSub, nil)
		var list []map[string]any
		_ = json.Unmarshal(body, &list)
		var real, test int
		for _, e := range list {
			if e["test_run"] == true {
				test++
				continue
			}
			real++
			if e["status"] != "running" {
				t.Errorf("real instance status = %v, want running", e["status"])
			}
			if c, _ := e["context"].(map[string]any); c["dept"] != "SALES" {
				t.Errorf("real instance context = %v, want dept=SALES", e["context"])
			}
		}
		if real != 1 || test != 1 {
			t.Fatalf("instances: %d real, %d test runs, want 1 and 1: %s", real, test, body)
		}
		task := scenarioTasks(f.ownerSub)[0]
		if status, body := f.do(t, "POST", "/api/tasks/"+task["id"].(string)+"/complete", f.ownerSub, map[string]any{"decision": "complete", "comment": ""}); status != http.StatusOK {
			t.Fatalf("complete: %d %s", status, body)
		}
		_, body = f.do(t, "GET", "/api/developer/workflows/"+defID+"/instances", devSub, nil)
		_ = json.Unmarshal(body, &list)
		for _, e := range list {
			if e["test_run"] != true && e["status"] != "completed" {
				t.Errorf("after completion the list still says %v (the old query hardcoded 'running')", e["status"])
			}
		}
	})

	t.Run("legacy submit goes through the engine and never starts a draft", func(t *testing.T) {
		status, res := createDef("Newest draft")
		if status != http.StatusOK {
			t.Fatalf("create draft: %d %v", status, res)
		}
		draftID, _ := res["id"].(string)
		// Newest PUBLISHED def is "Scenario", whose required dept is missing
		// from the legacy body → the engine refuses with 400. The old code
		// picked "Newest draft" (a draft!) and inserted its (empty) steps.
		status, body := f.do(t, "POST", "/api/workflow/submit", f.ownerSub, map[string]any{"model_id": f.modelID, "revision_id": f.revID})
		if status != http.StatusBadRequest {
			t.Errorf("legacy submit: %d %s (want 400 from ResolveStartContext)", status, body)
		}
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`, draftID).Scan(&n); err != nil || n != 0 {
			t.Errorf("legacy submit started %d instance(s) of a DRAFT definition (err %v)", n, err)
		}
	})
}
