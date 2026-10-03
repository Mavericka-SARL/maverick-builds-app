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
	"context"
	"encoding/json"
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
	p, err := f.proposals.CreateProposal(f.ctx, sess.ID, steps)
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

func TestRejectionStopsAfterThreeTries(t *testing.T) {
	c := proposalCheck{problems: []string{"step 1 (create_metric — x): boom"}, unchecked: []int{3}}
	if r := c.rejection(1); !strings.Contains(r, "call propose_actions again") || !strings.Contains(r, "Steps 3 could not be checked") {
		t.Errorf("first rejection: %s", r)
	}
	if r := c.rejection(maxProposalRejections); !strings.Contains(r, "Do not propose again in this turn") {
		t.Errorf("last rejection: %s", r)
	}
}
