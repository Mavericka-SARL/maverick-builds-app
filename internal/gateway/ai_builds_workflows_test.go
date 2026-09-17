// Builds everything a workflow needs using nothing but the AI Developer —
// the business role its step is assigned to, the form and the integration
// that feed it, the definition, and the automation rules that start it —
// then walks the part that stays human (publish, fire, approve) and proves
// the instance completes and the requester is told.
//
// This is the counterpart of ai_builds_model_test.go for programme item 5.
// The parity gaps it exists to catch are the ones reading the tool list
// cannot show: a rule that can never fire, a step nobody can pick up, a
// form whose numbers land nowhere, an id the model could not have known.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

func TestAIDeveloperBuildsAWorkflowThatRuns(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", sql, err)
		}
		return n
	}

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('AIWorkflowCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Expenses', 'planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Expenses model') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID)

	user := func(sub, email, role string) string {
		id := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,$2,$3,$4::uuid) RETURNING id::text`, sub, email, sub, custID)
		exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,$2,$3::uuid)`, id, role, wsID)
		return id
	}
	devSub, requesterSub, financeSub := "aiwf-dev", "aiwf-requester", "aiwf-finance"
	devID := user(devSub, "dev@aiwf.co", "developer")
	requesterID := user(requesterSub, "requester@aiwf.co", "business_user")
	financeID := user(financeSub, "finance@aiwf.co", "business_user")

	// ── The proposal, in the shape prompt.go teaches ───────────────────────
	var steps []map[string]any
	add := func(tool, description string, params map[string]any) int {
		steps = append(steps, proposeStep(tool, description, params))
		return len(steps)
	}
	role := add("create_business_role", "Create business role 'Finance Review'", map[string]any{"name": "Finance Review"})
	metric := add("create_metric", "Input metric 'expense'", map[string]any{"name": "expense", "is_input": true, "agg_rule": "sum", "format": "number"})
	dim := add("create_dimension", "Create dimension 'department'", map[string]any{"name": "department"})
	add("add_dimension_member", "Add 'Sales'", map[string]any{"dimension_id": ref(dim), "code": "SALES", "label": "Sales"})
	form := add("create_form_def", "Create form 'expense_request'", map[string]any{
		"name": "expense_request", "label": "Expense request",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "type": "number", "required": true},
			{"name": "department", "label": "Department", "type": "dimension", "dimension_id": ref(dim), "required": true},
		},
	})
	add("create_form_integration", "Post 'expense_request' amounts into 'expense'", map[string]any{
		"form_id": ref(form), "name": "Post expenses", "source_field": "amount", "target_metric_id": ref(metric),
		// A placeholder as a KEY: the dimension did not exist when this was written.
		"dimension_mappings": map[string]string{ref(dim): "department"},
	})
	wf := add("create_workflow_def", "Create workflow 'Expense Approval'", map[string]any{
		"name": "Expense Approval", "description": "Finance reviews requests over 1000", "trigger_event": "form_submit",
	})
	add("update_workflow_def", "Add the steps to 'Expense Approval'", map[string]any{
		"workflow_def_id": ref(wf), "single_active_instance": false,
		"context_schema": []map[string]any{{"key": "amount", "label": "Amount", "data_type": "Number", "required": true}},
		"steps": []map[string]any{
			{"id": "check-amount", "name": "Over 1000?", "type": "condition",
				"condition": map[string]any{"left": "amount", "operator": "greater_than", "right": 1000},
				"routes":    map[string]string{"true": "finance-review", "false": "notify-done"}},
			{"id": "finance-review", "name": "Finance Review", "type": "approval", "assignee_roles": []string{"Finance Review"},
				"sla_hours": 48, "required_comment": true,
				"routes": map[string]string{"approve": "notify-done", "reject": "end-rejected"}},
			{"id": "notify-done", "name": "Notify requester", "type": "notification",
				"notification": map[string]any{"recipient_type": "requester", "subject": "Expense request approved", "message": "Your expense request was approved."},
				"routes":       map[string]string{"next": "end-completed"}},
		},
	})
	manualRule := add("create_automation_rule", "Start 'Expense Approval' by hand", map[string]any{
		"name": "Submit expense", "trigger_type": "manual", "workflow_name": "Expense Approval",
	})
	add("create_automation_rule", "Start 'Expense Approval' when a request is submitted", map[string]any{
		"name": "Expense submitted", "trigger_type": "form_submit", "source_form_id": ref(form), "workflow_def_id": ref(wf),
	})
	_ = role

	args, _ := json.Marshal(map[string]any{"steps": steps})
	fake := &multiScriptProvider{resps: []providers.ChatResponse{{
		FinishReason: "tool_calls",
		Message: providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{
			{ID: "call_1", Name: "propose_actions", Arguments: args},
		}},
	}}}

	t.Setenv("DEV_MODE", "true")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, testProvider: fake}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)

	do := func(persona, method, path string, body any) (int, []byte) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Dev-User", persona)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.Bytes()
	}

	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, appID, devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled"); err != nil {
		t.Fatal(err)
	}
	pStore := aiassistant.NewProposalStore(pool)

	// 1. Propose, confirm.
	if status, body := do(devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "build the expense approval"}); status != http.StatusOK {
		t.Fatalf("send message: %d\n%s", status, body)
	}
	proposals, err := pStore.ListProposals(ctx, sess.ID)
	if err != nil || len(proposals) != 1 || proposals[0].Status != "pending" {
		t.Fatalf("proposals: %v %+v", err, proposals)
	}
	if n := count(`SELECT count(*) FROM workflow.workflow_def WHERE application_id=$1::uuid`, appID); n != 0 {
		t.Fatalf("%d workflow(s) existed before confirmation", n)
	}
	if status, body := do(devSub, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposals[0].ID+"/confirm", nil); status != http.StatusOK {
		t.Fatalf("confirm: %d\n%s", status, body)
	}
	confirmed, _ := pStore.GetProposal(ctx, proposals[0].ID)
	if confirmed.Status != "executed" {
		for i, s := range confirmed.Steps {
			if s.Status != "success" {
				t.Errorf("step %d (%s) %s: %s", i+1, s.Tool, s.Status, s.Result)
			}
		}
		t.Fatalf("proposal finished %q, want executed", confirmed.Status)
	}
	created := func(step int) string { return confirmed.Steps[step-1].CreatedID }
	wfID, manualRuleID, formID := created(wf), created(manualRule), created(form)
	draftRev := q(`SELECT COALESCE(draft_revision_id::text,'') FROM ai_assistant.session WHERE id=$1::uuid`, sess.ID)
	if draftRev == "" || draftRev == revID {
		t.Fatalf("draft revision %q — AI writes must not target the active revision", draftRev)
	}

	// 2. What landed, and how it is wired.
	if n := count(`SELECT count(*) FROM identity.business_role WHERE workspace_id=$1::uuid AND name='Finance Review'`, wsID); n != 1 {
		t.Errorf("business roles named Finance Review in the app's workspace: %d", n)
	}
	var wfStatus string
	var single bool
	var stepCount int
	if err := pool.QueryRow(ctx, `SELECT status, single_active_instance, jsonb_array_length(steps) FROM workflow.workflow_def WHERE id=$1::uuid`, wfID).Scan(&wfStatus, &single, &stepCount); err != nil {
		t.Fatal(err)
	}
	if wfStatus != "draft" || single || stepCount != 3 {
		t.Errorf("workflow status=%s single_active_instance=%v steps=%d — want draft/false/3", wfStatus, single, stepCount)
	}
	var mapped []byte
	var targetName string
	if err := pool.QueryRow(ctx, `
		SELECT fm.dimension_mappings, md.name FROM model.form_metric_mapping fm
		JOIN model.metric_def md ON md.id = fm.target_metric_id
		WHERE fm.form_id=$1::uuid`, formID).Scan(&mapped, &targetName); err != nil {
		t.Fatalf("integration: %v", err)
	}
	dimID := q(`SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='department'`, modelID, draftRev)
	if targetName != "expense" || string(mapped) != `{"`+dimID+`": "department"}` {
		t.Errorf("integration posts to %q with mappings %s", targetName, mapped)
	}
	// Both rules bind by name: the definition is a draft, and the assistant
	// cannot publish. The form rule carries its source form.
	var byIDCount, byNameCount, withForm int
	_ = pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE workflow_def_id IS NOT NULL), count(*) FILTER (WHERE workflow_def_id IS NULL AND workflow_name='Expense Approval'),
		       count(*) FILTER (WHERE source_form_id::text = $2)
		FROM workflow.automation_rule WHERE application_id=$1::uuid`, appID, formID).Scan(&byIDCount, &byNameCount, &withForm)
	if byIDCount != 0 || byNameCount != 2 || withForm != 1 {
		t.Errorf("rules: by id %d, by name %d, with the form %d — want 0/2/1", byIDCount, byNameCount, withForm)
	}

	// 3. The human gate. Before publish the rule exists but cannot fire —
	//    and says why, rather than starting a draft.
	if status, body := do(devSub, "POST", "/api/ai/sessions/"+sess.ID+"/promote-draft", nil); status != http.StatusOK {
		t.Fatalf("promote: %d\n%s", status, body)
	}
	if status, body := do(requesterSub, "POST", "/api/automation/trigger/"+manualRuleID, map[string]any{"payload": map[string]string{"amount": "2500"}}); status != http.StatusBadRequest || !strings.Contains(string(body), "not published") {
		t.Fatalf("firing before publish: %d %s — want 400 'not published'", status, body)
	}
	if status, body := do(devSub, "POST", "/api/developer/workflows/"+wfID+"/publish", nil); status != http.StatusOK {
		t.Fatalf("publish (the console's own validation gate): %d\n%s", status, body)
	}
	exec(`INSERT INTO identity.business_role_member (role_id, user_id) SELECT id, $2::uuid FROM identity.business_role WHERE workspace_id=$1::uuid AND name='Finance Review'`, wsID, financeID)

	// 4. Fire it as the requester. 2500 > 1000 routes to Finance Review.
	status, body := do(requesterSub, "POST", "/api/automation/trigger/"+manualRuleID, map[string]any{"payload": map[string]string{"amount": "2500"}})
	if status != http.StatusOK {
		t.Fatalf("fire: %d\n%s", status, body)
	}
	var execResp struct {
		InstanceID string `json:"instance_id"`
	}
	_ = json.Unmarshal(body, &execResp)
	if execResp.InstanceID == "" {
		t.Fatalf("no instance started: %s", body)
	}
	if n := count(`SELECT count(*) FROM workflow.workflow_step WHERE instance_id=$1::uuid AND step_def_id='finance-review' AND status='in_progress'`, execResp.InstanceID); n != 1 {
		t.Fatalf("Finance Review is not the active step (condition did not route)")
	}

	// 5. Approve as a member of the AI-created role, through the inbox.
	status, body = do(financeSub, "GET", "/api/tasks", nil)
	if status != http.StatusOK {
		t.Fatalf("tasks: %d\n%s", status, body)
	}
	var tasks []map[string]any
	_ = json.Unmarshal(body, &tasks)
	var stepID string
	for _, tk := range tasks {
		if tk["step_def_id"] == "finance-review" && tk["instance_id"] == execResp.InstanceID {
			stepID, _ = tk["id"].(string)
		}
	}
	if stepID == "" {
		t.Fatalf("the Finance Review task is not in the role member's inbox: %s", body)
	}
	if status, body := do(financeSub, "POST", "/api/tasks/"+stepID+"/complete", map[string]string{"decision": "approve", "comment": "Within policy."}); status != http.StatusOK {
		t.Fatalf("approve: %d\n%s", status, body)
	}

	// 6. It ran to the end, and the requester was told.
	var instStatus string
	_ = pool.QueryRow(ctx, `SELECT status::text FROM workflow.workflow_instance WHERE id=$1::uuid`, execResp.InstanceID).Scan(&instStatus)
	if instStatus != "completed" {
		t.Errorf("instance status = %q, want completed", instStatus)
	}
	if n := count(`SELECT count(*) FROM notification.notification WHERE recipient_user_id=$1::uuid AND template_vars->>'subject'='Expense request approved'`, requesterID); n != 1 {
		t.Errorf("requester notifications with the approval subject: %d, want 1", n)
	}
	fmt.Println("ai-built workflow ran end to end:", execResp.InstanceID)
}
