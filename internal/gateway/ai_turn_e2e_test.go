package gateway

// One AI Developer turn's limits and checks, end to end over the real SSE
// loop (ai_turn.go): plans corrected while their failures fall, then the
// passing steps, then a stop — never a question; formulas that run but look
// wrong sent back once, then shown with the warning; a promoted session
// refused; and a turn stopped by its time limit.

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

type aiTurnEnv struct {
	chat    *aiassistant.ChatStore
	session string
	send    func(content string) (int, string)
}

func newAITurnEnv(t *testing.T, provider providers.Provider) aiTurnEnv {
	t.Helper()
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
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('TurnCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	if _, err := pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID); err != nil {
		t.Fatal(err)
	}
	devSub := "turn-dev"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev@turnco.com', 'Dev', $2::uuid) RETURNING id::text`, devSub, custID)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, devID, wsID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_MODE", "true")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, testProvider: provider}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)
	chat := aiassistant.NewChatStore(pool)
	sess, err := chat.CreateSession(ctx, appID, modelID, devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chat.SetTitleIfEmpty(ctx, sess.ID, "pre-titled")
	return aiTurnEnv{chat: chat, session: sess.ID, send: func(content string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"content": content})
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/ai/sessions/"+sess.ID+"/messages", bytes.NewReader(body))
		req.Header.Set("X-Dev-User", devSub)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.String()
	}}
}

func proposeResp(id string, steps ...map[string]any) providers.ChatResponse {
	args, _ := json.Marshal(map[string]any{"steps": steps})
	return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
		ToolCalls: []providers.ToolCall{{ID: id, Name: "propose_actions", Arguments: args}}}}
}

func turnMetricStep(name string, params map[string]any) map[string]any {
	params["name"] = name
	return map[string]any{"tool": "create_metric", "description": "Create " + name, "params": params}
}

// toolResults are the tool-role messages of a session, in order.
func toolResults(t *testing.T, env aiTurnEnv) []string {
	t.Helper()
	msgs, err := env.chat.ListMessages(context.Background(), env.session)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range msgs {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	return out
}

func TestSendMessage_RetriesWhileFailuresFallThenPassingStepsThenStop(t *testing.T) {
	bad := func(i int) map[string]any {
		return turnMetricStep(fmt.Sprintf("bad%d", i), map[string]any{"formula": "no_such_metric * 2"})
	}
	good := turnMetricStep("good", map[string]any{"is_input": true})
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		proposeResp("c1", good, bad(1), bad(2), bad(3)), // 3 fail
		proposeResp("c2", good, bad(1), bad(2)),         // 2: falling
		proposeResp("c3", good, bad(1)),                 // 1: falling, past the 3 always allowed
		proposeResp("c4", good, bad(1)),                 // 1: not falling → the passing steps
		proposeResp("c5", good, bad(2)),                 // 1 again → stop
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "bad1 fails: no_such_metric does not exist."}},
	}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build stage 4"); status != http.StatusOK || !strings.Contains(sse, "event: done") {
		t.Fatalf("send: %d\n%s", status, sse)
	}
	results := toolResults(t, env)
	want := []string{"Fix every failing step", "Fix every failing step", "Fix every failing step", "ONLY the steps that pass", "Do not propose again"}
	if len(results) != len(want) {
		t.Fatalf("%d tool results, want %d: %q", len(results), len(want), results)
	}
	for i, w := range want {
		if !strings.Contains(results[i], w) {
			t.Errorf("attempt %d's result does not say %q:\n%s", i+1, w, results[i])
		}
		if strings.Contains(results[i], "ask how to proceed") {
			t.Errorf("attempt %d tells the model to ask", i+1)
		}
	}
}

// The proposal of only the passing steps comes with the plan check's list of
// what it left out, whatever the model's reply says.
func TestSendMessage_PassingStepsComeWithWhatWasLeftOut(t *testing.T) {
	bad := func(n int) map[string]any {
		return turnMetricStep(fmt.Sprintf("bad%d", n), map[string]any{"formula": "no_such_metric * 2"})
	}
	good := turnMetricStep("good", map[string]any{"is_input": true})
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		proposeResp("c1", good, bad(1)), proposeResp("c2", good, bad(2)), proposeResp("c3", good, bad(1)), // → the passing steps
		proposeResp("c4", good),
	}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build it"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s", status, sse)
	}
	msgs, _ := env.chat.ListMessages(context.Background(), env.session)
	found := false
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.Content, "left out of this proposal") && strings.Contains(m.Content, "bad1") {
			found = true
		}
	}
	if !found {
		t.Errorf("no message tells the developer that bad1 was left out")
	}
}

func TestSendMessage_WarnsOnceThenShowsWithWarnings(t *testing.T) {
	plan := []map[string]any{
		{"tool": "create_dimension", "description": "Regions", "params": map[string]any{"name": "mix_region",
			"members": []map[string]any{{"code": "NA", "label": "NA"}, {"code": "EU", "label": "EU"}}}},
		{"tool": "create_dimension", "description": "Products", "params": map[string]any{"name": "mix_product",
			"members": []map[string]any{{"code": "ALL_P", "label": "All"}, {"code": "SN", "label": "Snacks", "parent_code": "ALL_P"},
				{"code": "BV", "label": "Beverages", "parent_code": "ALL_P"}}}},
		turnMetricStep("weighted_base", map[string]any{"is_input": true}),
		turnMetricStep("growth_pct", map[string]any{"is_input": true, "format": "percentage", "agg_rule": "average"}),
		{"tool": "create_grid", "description": "Mix grid", "params": map[string]any{"name": "Mix",
			"dimensions": []string{"mix_region", "mix_product"}, "metrics": []string{"weighted_base", "growth_pct"}}},
		turnMetricStep("target", map[string]any{"formula": "weighted_base * growth_pct"}),
		turnMetricStep("share", map[string]any{"formula": "weighted_base / SUMIFS(weighted_base, mix_region, mix_region)", "agg_rule": "formula"}),
		{"tool": "add_grid_metric", "description": "Target on Mix", "params": map[string]any{"grid_id": "<created in step 5>", "metric_id": "<created in step 6>"}},
		{"tool": "add_grid_metric", "description": "Share on Mix", "params": map[string]any{"grid_id": "<created in step 5>", "metric_id": "<created in step 7>"}},
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		proposeResp("c1", plan...),
		proposeResp("c2", plan...), // unchanged: intended
	}}
	env := newAITurnEnv(t, fake)
	status, sse := env.send("build the mix")
	if status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s\ntool results: %q", status, sse, toolResults(t, env))
	}
	results := toolResults(t, env)
	if len(results) < 1 || !strings.Contains(results[0], "Proposal NOT shown yet: it runs, but these steps look wrong") {
		t.Fatalf("the first plan was not sent back with warnings: %q", results)
	}
	for _, w := range []string{"growth_pct, a Percentage metric", "without dividing by 100",
		`LOOKUP(weighted_base, mix_product, "ALL_P")`, "the cell's own value"} {
		if !strings.Contains(results[0], w) {
			t.Errorf("the warnings lack %q:\n%s", w, results[0])
		}
	}
	if !strings.Contains(sse, `"warnings":[`) || !strings.Contains(sse, "without dividing by 100") {
		t.Errorf("the proposal event does not carry the warnings for the developer:\n%s", sse)
	}
}

// A plan that runs goes back once for its warnings; a model that then asks
// "shall I propose it?" instead of proposing it again does not lose it: the
// turn ends showing that plan with its warnings.
func TestSendMessage_WarnedPlanShownWhenTheModelOnlyAsks(t *testing.T) {
	plan := []map[string]any{
		turnMetricStep("weighted_base", map[string]any{"is_input": true}),
		turnMetricStep("growth_pct", map[string]any{"is_input": true, "format": "percentage", "agg_rule": "average"}),
		turnMetricStep("target", map[string]any{"formula": "weighted_base * growth_pct"}),
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		proposeResp("c1", plan...),
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "I'm ready to propose the full plan. Confirm and I will post it."}},
	}}
	env := newAITurnEnv(t, fake)
	status, sse := env.send("build the targets")
	if status != http.StatusOK || !strings.Contains(sse, "event: proposal") || !strings.Contains(sse, "without dividing by 100") {
		t.Fatalf("the warned plan was not shown with its warnings: %d\n%s", status, sse)
	}
	msgs, _ := env.chat.ListMessages(context.Background(), env.session)
	if last := msgs[len(msgs)-1]; last.Role != "assistant" || !strings.Contains(last.Content, "The plan below runs (3 step(s))") {
		t.Errorf("the transcript ends with %q, want the note that the plan is shown with its warnings", last.Content)
	}
}

// A plan that fails its check goes back; a model that then only asks is
// told once to propose, and its corrected plan reaches the developer.
func TestSendMessage_NudgedToProposeAfterASendBack(t *testing.T) {
	base := turnMetricStep("base", map[string]any{"is_input": true})
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		proposeResp("c1", base, turnMetricStep("target", map[string]any{"formula": "no_such_metric * 2"})),
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "I will fix it. Confirm and I will post the plan."}},
		proposeResp("c2", base, turnMetricStep("target", map[string]any{"formula": "base * 2"})),
	}}
	env := newAITurnEnv(t, fake)
	status, sse := env.send("add the target")
	if status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("the corrected plan was not proposed after the nudge: %d calls=%d\n%s\nresults %q", status, fake.calls, sse, toolResults(t, env))
	}
	if fake.calls != 3 {
		t.Errorf("model calls = %d, want 3 (plan, question, corrected plan)", fake.calls)
	}
}

func TestSendMessage_PromotedSessionIsFinished(t *testing.T) {
	env := newAITurnEnv(t, &scriptedProvider{resp: providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "hi"}}})
	if _, err := env.chat.MarkPromoted(context.Background(), env.session); err != nil {
		t.Fatal(err)
	}
	status, body := env.send("update a metric")
	if status != http.StatusConflict || !strings.Contains(body, "SESSION_PROMOTED") || !strings.Contains(body, "start a new session") {
		t.Errorf("a message in a promoted session: %d %s, want 409 SESSION_PROMOTED saying to start a new session", status, body)
	}
}

// blockingProvider waits until its request is cancelled — a model call that
// would run for minutes.
type blockingProvider struct{}

func (blockingProvider) Chat(ctx context.Context, _ providers.ChatRequest) (providers.ChatResponse, error) {
	<-ctx.Done()
	return providers.ChatResponse{}, ctx.Err()
}

func TestSendMessage_TurnTimeLimit(t *testing.T) {
	t.Setenv("AI_TURN_TIMEOUT", "300ms")
	env := newAITurnEnv(t, blockingProvider{})
	status, sse := env.send("type every value of the sheet")
	if status != http.StatusOK || !strings.Contains(sse, "event: error") || !strings.Contains(sse, "Stopped after") {
		t.Fatalf("a turn past its limit: %d\n%s", status, sse)
	}
	msgs, _ := env.chat.ListMessages(context.Background(), env.session)
	if last := msgs[len(msgs)-1]; last.Role != "assistant" || !strings.Contains(last.Content, "import_file_data") {
		t.Errorf("the transcript ends with %q, want the stop and its advice", last.Content)
	}
}

func TestAsksToPropose(t *testing.T) {
	for reply, want := range map[string]bool{
		"I'm ready to propose the full plan. Confirm and I will post it.":                      true,
		"If you want me to proceed I will prepare a corrected proposal. Which would you like?": true,
		"Confirm you still want me to proceed and I'll post the full proposal.":                true,
		"FR is not a country in the model.":                                                    false,
		"Those properties do not exist.":                                                       false,
	} {
		if got := asksToPropose(reply); got != want {
			t.Errorf("asksToPropose(%q) = %v, want %v", reply, got, want)
		}
	}
}

// A passing plan that lacks more than the failing steps names the rest too.
func TestDroppedStepsNamesWhatThePartialPlanLacks(t *testing.T) {
	st := func(tool, desc, params string) aiassistant.ProposalStep {
		return aiassistant.ProposalStep{Tool: tool, Description: desc, Params: json.RawMessage(params)}
	}
	failing := []aiassistant.ProposalStep{
		st("create_dashboard", "README", `{"name": "README"}`),
		st("update_dashboard_widget", "Move a note", `{"widget_id": "x", "dashboard_id": "y"}`),
		st("create_dashboard", "Plan Summary", `{"name":"Plan Summary"}`),
		st("create_dashboard", "HR Dashboard", `{"name":"HR Dashboard"}`),
	}
	passing := []aiassistant.ProposalStep{st("create_dashboard", "README", `{"name":"README"}`)}
	got := droppedSteps(failing, passing, []string{`step 2 (update_dashboard_widget — Move a note): this tool does not read "dashboard_id"`})
	want := []string{"step 3 (create_dashboard — Plan Summary)", "step 4 (create_dashboard — HR Dashboard)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("dropped = %q, want %q", got, want)
	}
	if note := leftOutNote([]string{"step 2 (…)"}, got); !strings.Contains(note, "Also left out") || !strings.Contains(note, "HR Dashboard") {
		t.Errorf("the note does not name the dropped steps:\n%s", note)
	}
}

// The recurring mistakes of the HR rebuild go back once as warnings: a typed
// counter of 1s, a headcount summed over months, a summary grid widget with
// every metric of a large grid.
func TestSendMessage_WarnsOfRecurringMistakes(t *testing.T) {
	members := []map[string]any{}
	for _, c := range []string{"E1", "E2", "E3", "E4", "E5"} {
		members = append(members, map[string]any{"code": c, "label": c})
	}
	ones := []map[string]any{}
	for _, c := range []string{"E1", "E2", "E3", "E4", "E5"} {
		ones = append(ones, map[string]any{"members": map[string]string{"staff": c}, "value": 1})
	}
	many := []string{}
	plan := []map[string]any{
		{"tool": "create_dimension", "description": "Staff", "params": map[string]any{"name": "staff", "members": members}},
		{"tool": "create_dimension", "description": "Months", "params": map[string]any{"name": "period", "dimension_type": "time",
			"time_granularity": "month", "fiscal_year_start_month": 1, "members": []map[string]any{{"code": "FY27", "label": "FY27"}}}},
		{"tool": "generate_time_members", "description": "Months", "params": map[string]any{"dimension_id": "period", "start": "2027-01-01", "end": "2027-03-31", "parent_code": "FY27"}},
		turnMetricStep("emp_count", map[string]any{"is_input": true}),
		{"tool": "create_grid", "description": "Staff", "params": map[string]any{"name": "Staff", "dimensions": []string{"staff"}, "metrics": []string{"emp_count"}}},
		{"tool": "write_input_values", "description": "Count", "params": map[string]any{"metric_id": "emp_count", "values": ones}},
		turnMetricStep("ending_hc", map[string]any{"is_input": true}),
	}
	for i := 0; i < 13; i++ {
		n := fmt.Sprintf("plan_m%d", i)
		many = append(many, n)
		plan = append(plan, turnMetricStep(n, map[string]any{"is_input": true}))
	}
	plan = append(plan,
		map[string]any{"tool": "create_grid", "description": "Plan", "params": map[string]any{"name": "Plan", "dimensions": []string{"period"}, "metrics": append([]string{"ending_hc"}, many...)}},
		map[string]any{"tool": "create_dashboard", "description": "Summary", "params": map[string]any{"name": "Summary"}},
		map[string]any{"tool": "add_dashboard_widget", "description": "Summary grid", "params": map[string]any{"dashboard_id": "Summary", "widget_type": "grid", "ref_id": "Plan",
			"title": "Monthly Summary", "pos_x": 0, "pos_y": 0, "size_w": 600, "size_h": 300}},
	)
	fake := &multiScriptProvider{resps: []providers.ChatResponse{proposeResp("c1", plan...), proposeResp("c2", plan...)}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build it"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s\ntool results: %q", status, sse, toolResults(t, env))
	}
	results := toolResults(t, env)
	if len(results) < 1 {
		t.Fatal("no tool result")
	}
	for _, w := range []string{"every one of the 5 values of emp_count is 1", "COUNTIFS",
		"ending_hc looks like a level", `time_summary "last"`, `the grid widget "Monthly Summary" shows all 14 metrics of Plan`} {
		if !strings.Contains(results[0], w) {
			t.Errorf("the warnings lack %q:\n%s", w, results[0])
		}
	}
}
