// Reported live: the AI Developer proposed calculated metrics that named a
// dimension by a guessed spelling (setup_item for "Setup Item") and read a
// property as though it were a dimension (p_and_l_line, a property of
// "Cost Center"); confirming failed them, and the steps built on them
// failed too. A plan is now run against the model before the developer
// sees it, unknown names say what they most likely meant, a dimension whose
// name is not an identifier is written {Setup Item} — its property
// {Cost Center}.p_and_l_line — and confirming stops at the first failure.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
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

type planFixture struct {
	ctx                                  context.Context
	h                                    *handler
	srv                                  *httptest.Server
	appID, modelID, revID, devID, devSub string
	chat                                 *aiassistant.ChatStore
	proposals                            *aiassistant.ProposalStore
	q                                    func(sql string, args ...any) string
	count                                func(sql string, args ...any) int
}

func newPlanFixture(t *testing.T, provider providers.Provider) *planFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	f := &planFixture{ctx: ctx, devSub: "plan-dev"}
	f.q = func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	f.count = func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", sql, err)
		}
		return n
	}
	q := f.q
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('PlanCo', 'enterprise') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, cust)
	f.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'FP&A', 'planning') RETURNING id::text`, ws, cust)
	f.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'FP&A') RETURNING id::text`, f.appID)
	f.revID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelID)
	q(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid RETURNING id::text`, f.revID, f.modelID)
	dim := func(name string) string {
		return q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,$3,'standard') RETURNING id::text`, f.modelID, f.revID, name)
	}
	setup, cc := dim("Setup Item"), dim("Cost Center")
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid,'FX','FX rate') RETURNING id::text`, setup)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label, properties) VALUES ($1::uuid,'S100','Sales','{"p_and_l_line":"Revenue"}') RETURNING id::text`, cc)
	q(`INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid,'p_and_l_line','text') RETURNING id::text`, cc)
	q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid,$2::uuid,'amount',true,'sum') RETURNING id::text`, f.modelID, f.revID)
	f.devID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,'dev@plan.co','Dev',$2::uuid) RETURNING id::text`, f.devSub, cust)
	q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'developer',$2::uuid) RETURNING id::text`, f.devID, ws)

	t.Setenv("DEV_MODE", "true")
	f.h = &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, testProvider: provider}
	mux := http.NewServeMux()
	f.h.registerRoutes(mux, nil)
	f.srv = httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(f.srv.Close)
	f.chat, f.proposals = aiassistant.NewChatStore(pool), aiassistant.NewProposalStore(pool)
	return f
}

func (f *planFixture) post(t *testing.T, path string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(f.ctx, "POST", f.srv.URL+path, strings.NewReader(string(b)))
	req.Header.Set("X-Dev-User", f.devSub)
	req.Header.Set("X-App-Id", f.appID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	out := new(strings.Builder)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		out.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, out.String()
}

func (f *planFixture) session(t *testing.T) aiassistant.Session {
	t.Helper()
	sess, err := f.chat.CreateSession(f.ctx, f.appID, f.modelID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.chat.SetTitleIfEmpty(f.ctx, sess.ID, "pre-titled")
	return sess
}

func metricStep(name, formula string) map[string]any {
	return proposeStep("create_metric", "Create '"+name+"'", map[string]any{"name": name, "formula": formula, "is_input": false, "format": "number"})
}

func TestAPlanThatWouldFailGoesBackToTheAssistant(t *testing.T) {
	propose := func(steps ...map[string]any) providers.ChatResponse {
		b, _ := json.Marshal(map[string]any{"steps": steps})
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: "call_propose", Name: "propose_actions", Arguments: b}}}}
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		// The plan as it was proposed live.
		propose(
			metricStep("setup_fx", `IF(setup_item = "FX", amount, 0)`),
			metricStep("revenue_total", `SUMIFS(amount, p_and_l_line, "Revenue")`),
			metricStep("revenue_half", `revenue_total / 2`),
		),
		// Corrected from the check's errors.
		propose(
			metricStep("setup_fx", `IF({Setup Item} = "FX", amount, 0)`),
			metricStep("revenue_total", `SUMIFS(amount, {Cost Center}.p_and_l_line, "Revenue")`),
			metricStep("revenue_half", `revenue_total / 2`),
		),
	}}
	f := newPlanFixture(t, fake)
	sess := f.session(t)
	metrics := func() int {
		return f.count(`SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid`, f.modelID)
	}

	status, body := f.post(t, "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "build the FP&A summary metrics"})
	if status != http.StatusOK || !strings.Contains(body, "event: proposal") {
		t.Fatalf("message: %d %s", status, body)
	}
	msgs, _ := f.chat.ListMessages(f.ctx, sess.ID)
	var rejection string
	for _, m := range msgs {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "Proposal NOT shown") {
			rejection = m.Content
		}
	}
	for _, want := range []string{
		`step 1 (create_metric — Create 'setup_fx')`,
		`the dimension is named "Setup Item": write it as {Setup Item}`,
		`step 2 (create_metric — Create 'revenue_total')`,
		`p_and_l_line is a property of dimension "Cost Center"`,
		`{Cost Center}.p_and_l_line`,
		`step 3 (create_metric — Create 'revenue_half')`, // its source never got created
		"propose_actions again with the WHOLE corrected plan",
	} {
		if !strings.Contains(rejection, want) {
			t.Errorf("the check's answer lacks %q:\n%s", want, rejection)
		}
	}

	// Only the corrected plan became a proposal, and checking wrote nothing.
	pending, err := f.proposals.ListPendingProposals(f.ctx, sess.ID)
	if err != nil || len(pending) != 1 || !strings.Contains(string(pending[0].Steps[0].Params), "{Setup Item}") {
		t.Fatalf("pending proposals: %v %+v", err, pending)
	}
	if n := metrics(); n != 1 {
		t.Fatalf("the dry runs left %d metric(s), want just amount", n)
	}
	if status, body := f.post(t, "/api/ai/sessions/"+sess.ID+"/proposals/"+pending[0].ID+"/confirm", nil); status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, body)
	}
	p, _ := f.proposals.GetProposal(f.ctx, pending[0].ID)
	if p.Status != "executed" {
		t.Fatalf("confirmed plan: %s %+v", p.Status, p.Steps)
	}
	s2, _ := f.chat.GetSession(f.ctx, sess.ID)
	if n := f.count(`SELECT count(*) FROM model.metric_def WHERE revision_id=$1::uuid AND name IN ('setup_fx','revenue_total','revenue_half')`, s2.DraftRevisionID); n != 3 {
		t.Fatalf("the draft has %d of the 3 metrics", n)
	}
}

// A plan that fails when confirmed — here past the check, stored directly —
// stops at its first failing step; nothing after it runs.
func TestConfirmStopsAtTheFirstFailingStep(t *testing.T) {
	f := newPlanFixture(t, &multiScriptProvider{})
	sess := f.session(t)
	var steps []aiassistant.ProposalStep
	for _, s := range []map[string]any{
		metricStep("ok_first", `amount * 2`),
		metricStep("broken", `no_such_thing + 1`),
		metricStep("after_broken", `amount * 3`),
	} {
		b, _ := json.Marshal(s["params"])
		steps = append(steps, aiassistant.ProposalStep{Tool: s["tool"].(string), Description: s["description"].(string), Params: b})
	}
	p, err := f.proposals.CreateProposal(f.ctx, sess.ID, steps, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, body := f.post(t, "/api/ai/sessions/"+sess.ID+"/proposals/"+p.ID+"/confirm", nil)
	if status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, body)
	}
	got, _ := f.proposals.GetProposal(f.ctx, p.ID)
	var statuses []string
	for _, s := range got.Steps {
		statuses = append(statuses, s.Status)
	}
	if strings.Join(statuses, ",") != "success,failed,skipped" || got.Status != "partial" {
		t.Fatalf("steps %v, proposal %s", statuses, got.Status)
	}
	if n := f.count(`SELECT count(*) FROM model.metric_def WHERE name='after_broken'`); n != 0 {
		t.Fatal("a step after the failure ran")
	}
	if !strings.Contains(body, "step 2 failed, so the 1 after it were not run") {
		t.Errorf("summary: %s", body)
	}
}

// A failing plan is corrected three times, and more while each attempt
// fails fewer steps (up to maxFallingRejections); then the model proposes
// the steps that pass, and then it stops and says what fails. It is never
// told to ask how to proceed.
func TestRejectionPolicy(t *testing.T) {
	c := proposalCheck{problems: []string{"step 1 (create_metric — x): boom"}, unchecked: []int{3}}
	if r := c.rejection(retryFix); !strings.Contains(r, "call propose_actions again") || !strings.Contains(r, "Steps 3 could not be checked") {
		t.Errorf("a fix: %s", r)
	}
	if r := c.rejection(retryPartial); !strings.Contains(r, "ONLY the steps that pass") || strings.Contains(r, "ask how") {
		t.Errorf("the passing steps: %s", r)
	}
	if r := c.rejection(retryStop); !strings.Contains(r, "Do not propose again in this turn") || !strings.Contains(r, "do not ask whether") {
		t.Errorf("the stop: %s", r)
	}
	for _, tc := range []struct {
		problems []int
		want     []retryMode
	}{
		{[]int{5, 5, 5, 5, 5}, []retryMode{retryFix, retryFix, retryPartial, retryStop, retryStop}},
		{[]int{5, 4, 3, 2, 1, 1, 1}, []retryMode{retryFix, retryFix, retryFix, retryFix, retryFix, retryPartial, retryStop}},
		{[]int{9, 8, 7, 6, 5, 4, 3, 2, 1}, []retryMode{retryFix, retryFix, retryFix, retryFix, retryFix, retryFix, retryFix, retryPartial, retryStop}},
	} {
		var r planRetries
		for i, n := range tc.problems {
			if got := r.next(n); got != tc.want[i] {
				t.Errorf("failures %v: attempt %d gives %v, want %v", tc.problems, i+1, got, tc.want[i])
			}
		}
	}
}

// Workflows, forms, automation rules and file imports are checked too: their
// stores run on the check's transaction, so a mistake in them comes back to
// the assistant like any other — and the check leaves nothing behind.
func TestThePlanCheckCoversWorkflowsFormsAndFileImports(t *testing.T) {
	propose := func(steps ...map[string]any) providers.ChatResponse {
		b, _ := json.Marshal(map[string]any{"steps": steps})
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: "call_propose", Name: "propose_actions", Arguments: b}}}}
	}
	form := proposeStep("create_form_def", "Create form 'cost_request'", map[string]any{
		"name": "cost_request", "label": "Cost request",
		"fields": []map[string]any{{"name": "amount", "label": "Amount", "type": "number", "required": true}},
	})
	workflow := proposeStep("create_workflow_def", "Create workflow 'Cost Approval'", map[string]any{
		"name": "Cost Approval", "description": "Approve costs", "trigger_event": "manual",
	})
	rule := func(workflowName string) map[string]any {
		return proposeStep("create_automation_rule", "Start '"+workflowName+"' by hand", map[string]any{
			"name": "Submit cost", "trigger_type": "manual", "workflow_name": workflowName,
		})
	}
	grid := func(step int) []map[string]any {
		ref := fmt.Sprintf("<created in step %d>", step)
		return []map[string]any{
			proposeStep("create_grid", "Create grid 'Costs'", map[string]any{"name": "Costs"}),
			proposeStep("add_grid_dimension", "Cost Center on Costs", map[string]any{"grid_id": ref, "dimension_id": "Cost Center"}),
			proposeStep("add_grid_metric", "amount on Costs", map[string]any{"grid_id": ref, "metric_id": "amount"}),
		}
	}
	importStep := proposeStep("import_file_data", "Import costs.csv", map[string]any{
		"file": "costs.csv", "target_type": "grid", "target_id": "Costs", "column_map": map[string]string{"Cost Center": "Cost Center"},
	})
	addS200 := proposeStep("add_dimension_member", "Add S200", map[string]any{"dimension_id": "Cost Center", "code": "S200", "label": "Support"})
	reply := providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "ok"}}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		// 1. A rule naming a workflow that does not exist.
		propose(form, workflow, rule("Expense Review")),
		propose(form, workflow, rule("Cost Approval")),
		// 2. A file naming a member the plan does not add, then one it does.
		propose(append(grid(1), importStep)...),
		propose(append(append([]map[string]any{addS200}, grid(2)...), importStep)...),
		reply,
	}}
	f := newPlanFixture(t, fake)
	sess := f.session(t)
	rejections := func() []string {
		msgs, _ := f.chat.ListMessages(f.ctx, sess.ID)
		var out []string
		for _, m := range msgs {
			if m.Role == "tool" && strings.HasPrefix(m.Content, "Proposal NOT shown") {
				out = append(out, m.Content)
			}
		}
		return out
	}
	nothingLeft := func(when string) {
		t.Helper()
		for table, n := range map[string]int{
			"form defs":        f.count(`SELECT count(*) FROM model.form_def`),
			"workflow defs":    f.count(`SELECT count(*) FROM workflow.workflow_def`),
			"automation rules": f.count(`SELECT count(*) FROM workflow.automation_rule`),
			"grids":            f.count(`SELECT count(*) FROM model.grid_def`),
			"S200 members":     f.count(`SELECT count(*) FROM model.dimension_member WHERE code='S200'`),
		} {
			if n != 0 {
				t.Errorf("%s: the check left %d %s behind", when, n, table)
			}
		}
	}
	confirmPending := func() {
		t.Helper()
		pending, err := f.proposals.ListPendingProposals(f.ctx, sess.ID)
		if err != nil || len(pending) != 1 {
			t.Fatalf("pending proposals: %v %+v", err, pending)
		}
		if status, body := f.post(t, "/api/ai/sessions/"+sess.ID+"/proposals/"+pending[0].ID+"/confirm", nil); status != http.StatusOK {
			t.Fatalf("confirm: %d %s", status, body)
		}
		if p, _ := f.proposals.GetProposal(f.ctx, pending[0].ID); p.Status != "executed" {
			t.Fatalf("confirmed plan %s: %+v", p.Status, p.Steps)
		}
	}

	if status, body := f.post(t, "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "a cost approval workflow"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, body)
	}
	if r := rejections(); len(r) != 1 || !strings.Contains(r[0], "step 3 (create_automation_rule") || strings.Contains(r[0], "step 1") || strings.Contains(r[0], "could not be checked") {
		t.Fatalf("the check's answers: %q", r)
	}
	nothingLeft("after the workflow plan was checked")
	confirmPending()

	// The attachment for the grid import.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "costs.csv")
	_, _ = fw.Write([]byte("Cost Center,amount\nS100,5\nS200,7\n"))
	_ = mw.Close()
	req, _ := http.NewRequestWithContext(f.ctx, "POST", f.srv.URL+"/api/ai/sessions/"+sess.ID+"/documents", &body)
	req.Header.Set("X-Dev-User", f.devSub)
	req.Header.Set("X-App-Id", f.appID)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attach: status %d", resp.StatusCode)
	}
	draft, _ := f.chat.GetSession(f.ctx, sess.ID)
	gridsBefore := f.count(`SELECT count(*) FROM model.grid_def WHERE revision_id=$1::uuid`, draft.DraftRevisionID)

	if status, body := f.post(t, "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "import the costs"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, body)
	}
	r := rejections()
	if len(r) != 2 || !strings.Contains(r[1], "step 4 (import_file_data") || !strings.Contains(r[1], "S200") {
		t.Fatalf("the import check's answer: %q", r)
	}
	if n := f.count(`SELECT count(*) FROM model.grid_def WHERE revision_id=$1::uuid`, draft.DraftRevisionID); n != gridsBefore {
		t.Fatalf("the check left a grid behind")
	}
	if n := f.count(`SELECT count(*) FROM model.dimension_member WHERE code='S200'`); n != 0 {
		t.Fatal("the check left S200 behind")
	}
	confirmPending() // the plan that adds S200 first
	if n := f.count(`SELECT count(*) FROM runtime.fact_input WHERE revision_id=$1::uuid`, draft.DraftRevisionID); n != 2 {
		t.Fatalf("imported %d value(s), want 2", n)
	}
}
