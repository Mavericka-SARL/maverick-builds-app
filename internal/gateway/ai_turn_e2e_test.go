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
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/ee/aikeys"
	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/license"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

type aiTurnEnv struct {
	chat      *aiassistant.ChatStore
	docs      *aiassistant.DocumentStore
	proposals *aiassistant.ProposalStore
	session   string
	send      func(content string) (int, string)
	// post and get call the gateway as the session's developer.
	post func(path string, body []byte) (int, string)
	get  func(path string) (int, string)
	// do calls the gateway as the session's developer with any method; db is
	// the test database.
	do func(method, path string, body []byte) (int, string)
	db *pgxpool.Pool
	// confirm confirms a proposal of the session as the console does.
	confirm func(proposalID string) (int, string)
}

// opts adjust the handler before it serves (a licence, say).
func newAITurnEnv(t *testing.T, provider providers.Provider, opts ...func(*handler)) aiTurnEnv {
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
	for _, o := range opts {
		o(h)
	}
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
	do := func(method, path string, body []byte) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(body))
		req.Header.Set("X-Dev-User", devSub)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.String()
	}
	post := func(path string, body []byte) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+path, bytes.NewReader(body))
		req.Header.Set("X-Dev-User", devSub)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.String()
	}
	get := func(path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
		req.Header.Set("X-Dev-User", devSub)
		req.Header.Set("X-App-Id", appID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.String()
	}
	return aiTurnEnv{chat: chat, docs: aiassistant.NewDocumentStore(pool), proposals: aiassistant.NewProposalStore(pool), session: sess.ID,
		post: post, get: get, do: do, db: pool,
		confirm: func(proposalID string) (int, string) {
			return post("/api/ai/sessions/"+sess.ID+"/proposals/"+proposalID+"/confirm", []byte(`{}`))
		}, send: func(content string) (int, string) {
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
		proposeResp("c4", good, bad(1)),                 // 1: not falling → the server proposes the passing steps
	}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build stage 4"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s", status, sse)
	}
	results := toolResults(t, env)
	want := []string{"Fix every failing step", "Fix every failing step", "Fix every failing step", "Proposal created by the server with the 1 step(s) that pass"}
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
		turnMetricStep("growth_pct", map[string]any{"is_input": true, "format": "percentage", "agg_rule": "average", "time_summary": "average"}),
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
	env.storedWarnings(t, "without dividing by 100")
}

// storedWarnings checks that the session's proposal keeps the plan check's
// warnings, so a reopened session shows them with it.
func (e aiTurnEnv) storedWarnings(t *testing.T, want string) {
	t.Helper()
	ps, err := e.proposals.ListProposals(context.Background(), e.session)
	if err != nil || len(ps) != 1 {
		t.Fatalf("proposals: %d (err %v), want 1", len(ps), err)
	}
	if !strings.Contains(strings.Join(ps[0].Warnings, "\n"), want) {
		t.Errorf("the stored proposal's warnings %q lack %q", ps[0].Warnings, want)
	}
}

// A plan that runs goes back once for its warnings; a model that then asks
// "shall I propose it?" instead of proposing it again does not lose it: the
// turn ends showing that plan with its warnings.
func TestSendMessage_WarnedPlanShownWhenTheModelOnlyAsks(t *testing.T) {
	plan := []map[string]any{
		turnMetricStep("weighted_base", map[string]any{"is_input": true}),
		turnMetricStep("growth_pct", map[string]any{"is_input": true, "format": "percentage", "agg_rule": "average", "time_summary": "average"}),
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
	env.storedWarnings(t, "without dividing by 100")
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

// A session whose draft became the working revision goes on (owner,
// 2026-11-13: the developer is never pushed to start a new session).
func TestSendMessage_PromotedSessionGoesOn(t *testing.T) {
	env := newAITurnEnv(t, &scriptedProvider{resp: providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "hi"}}})
	if _, err := env.chat.MarkPromoted(context.Background(), env.session); err != nil {
		t.Fatal(err)
	}
	status, body := env.send("update a metric")
	if status != http.StatusOK || !strings.Contains(body, "event: done") || strings.Contains(body, "new session") {
		t.Errorf("a message in a promoted session: %d %s, want the turn to run", status, body)
	}
}

// blockingProvider waits until its request is cancelled — a model call that
// would run for minutes.
type blockingProvider struct{}

func (blockingProvider) Chat(ctx context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	if r, ok := sideCall(req); ok {
		return r, nil
	}
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
	// Read with its error, and checked before indexing: an empty read on a
	// busy machine panicked here and stopped the package (2026-11-07).
	msgs, err := env.chat.ListMessages(context.Background(), env.session)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("the transcript after the stop: %d messages, err=%v", len(msgs), err)
	}
	if last := msgs[len(msgs)-1]; last.Role != "assistant" || !strings.Contains(last.Content, "import_file_data") {
		t.Errorf("the transcript ends with %q, want the stop and its advice", last.Content)
	}
}

// A reply that says the model was changed, or that a proposal follows, is
// told apart from an answer that changes nothing.
func TestClaimsUnproposedChange(t *testing.T) {
	for reply, want := range map[string]bool{
		"I have created the Fleet metrics and placed them on the grid.":  true,
		"Next I will propose the P&L lines.":                             true,
		"The depreciation metric has been updated to use CUMULATE.":      true,
		"Depreciation is opening PP&E plus capex, over the useful life.": false,
		"Which scenario should the ramp read?":                           false,
	} {
		if got := claimsUnproposedChange(reply); got != want {
			t.Errorf("claimsUnproposedChange(%q) = %v, want %v", reply, got, want)
		}
	}
}

func TestAsksPermissionAndPointsAtPlan(t *testing.T) {
	for reply, want := range map[string]bool{
		"The stage is too large for one proposal (>100 steps). Shall I split it into parts?":                    true,
		"Rows 4-9 are built. Should I run compare_with_sheet now, or continue? This does not change the model.": true,
		"Do you want me to continue with the Cash Flow?":                                                        true,
		"Which month does the fiscal year start in?":                                                            false,
		"Two readings:\n1) Opening = last closing\n2) Opening = the input\nWhich one?":                          false,
	} {
		if got := asksPermission(reply); got != want {
			t.Errorf("asksPermission(%q) = %v, want %v", reply, got, want)
		}
	}
	for reply, want := range map[string]bool{
		"I've prepared the corrected plan — please confirm above.": true,
		"The plan is ready for your review.":                       true,
		"Please review and confirm.":                               true,
		"Revenue reads the price input.":                           false,
	} {
		if got := pointsAtPlan.MatchString(reply); got != want {
			t.Errorf("pointsAtPlan(%q) = %v, want %v", reply, got, want)
		}
	}
}

func TestAsksToPropose(t *testing.T) {
	for reply, want := range map[string]bool{
		"I'm ready to propose the full plan. Confirm and I will post it.":                      true,
		"If you want me to proceed I will prepare a corrected proposal. Which would you like?": true,
		"Confirm you still want me to proceed and I'll post the full proposal.":                true,
		"FR is not a country in the model.":                                                    false,
		"Those properties do not exist.":                                                       false,
		// A real question about the model, with options, is left for the developer.
		"Net revenue does not reconcile with the sales import. Which source wins?\n1) The GL categories\n2) The sales ledger\nWould you like me to use one of them?": false,
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
			"title": "Monthly Summary", "pos_x": 0, "pos_y": 0, "size_w": 600, "size_h": 640}}, // a box its 14 rows fit (the layout check)
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

// A plan whose formula reads a metric on no grid goes back as a problem: a
// metric on no grid is one value for the whole model, so ebitda read the
// model's whole gross profit in every month (restaurant rebuild,
// 2026-10-31). Placed on the grid, the same plan is proposed. A flag of
// typed 1s is no counter and an opening capex no level: neither is warned.
func TestSendMessage_RefusesReadOfMetricOnNoGrid(t *testing.T) {
	month := map[string]any{"tool": "create_dimension", "description": "Months", "params": map[string]any{"name": "period",
		"members": []map[string]any{{"code": "M1", "label": "M1"}, {"code": "M2", "label": "M2"}, {"code": "M3", "label": "M3"},
			{"code": "M4", "label": "M4"}, {"code": "M5", "label": "M5"}}}}
	ones := []map[string]any{}
	for _, c := range []string{"M1", "M2", "M3", "M4", "M5"} {
		ones = append(ones, map[string]any{"members": map[string]string{"period": c}, "value": 1})
	}
	plan := func(placed bool) []map[string]any {
		gpGrid := []string{"revenue", "seas_event", "opening_capex", "ebitda"}
		if placed {
			gpGrid = append(gpGrid, "gross_profit")
		}
		return []map[string]any{
			month,
			turnMetricStep("revenue", map[string]any{"is_input": true}),
			turnMetricStep("seas_event", map[string]any{"is_input": true}),
			turnMetricStep("opening_capex", map[string]any{"is_input": true}),
			turnMetricStep("gross_profit", map[string]any{"is_input": false, "formula": "revenue * 0.6"}),
			turnMetricStep("ebitda", map[string]any{"is_input": false, "formula": "gross_profit - 10"}),
			{"tool": "create_grid", "description": "P&L", "params": map[string]any{"name": "PnL", "dimensions": []string{"period"}, "metrics": gpGrid}},
			{"tool": "write_input_values", "description": "Events", "params": map[string]any{"metric_id": "seas_event", "values": ones}},
		}
	}
	// gross_profit is made by the plan and read by ebitda on PnL: the server
	// places it there itself instead of refusing the plan for a turn.
	fake := &multiScriptProvider{resps: []providers.ChatResponse{proposeResp("c1", plan(false)...), proposeResp("c2", plan(false)...)}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build it"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s\ntool results: %q", status, sse, toolResults(t, env))
	}
	results := toolResults(t, env)
	if len(results) < 1 || !strings.Contains(results[len(results)-1], "Added by the server: gross_profit placed on the grid PnL") {
		t.Fatalf("the plan was not given gross_profit's placement:\n%q", results)
	}
	for _, r := range results {
		if strings.Contains(r, "which is on no grid") {
			t.Errorf("the plan was refused for a placement the server can add: %s", r)
		}
	}
	props, err := env.proposals.ListProposals(context.Background(), env.session)
	if err != nil || len(props) == 0 {
		t.Fatalf("proposals: %v, %d", err, len(props))
	}
	placed := false
	for _, st := range props[len(props)-1].Steps {
		if st.Tool == "add_grid_metric" && strings.Contains(st.Description, "gross_profit") {
			placed = true
		}
	}
	if !placed {
		t.Errorf("the proposal has no add_grid_metric step for gross_profit: %+v", props[len(props)-1].Steps)
	}
}

// A plan that writes values with decimals into a metric shown with none is
// warned of: a USD 0.3m strategic increment read 0 in the assistant's sales
// model. A metric left at the default decimals is not.
func TestSendMessage_WarnsOfHiddenDecimals(t *testing.T) {
	plan := []map[string]any{
		{"tool": "create_dimension", "description": "Activities", "params": map[string]any{"name": "activity",
			"members": []map[string]any{{"code": "A1", "label": "A1"}, {"code": "A2", "label": "A2"}}}},
		turnMetricStep("increment", map[string]any{"is_input": true, "format": "currency", "format_currency": "€", "format_decimals": 0}),
		turnMetricStep("rate", map[string]any{"is_input": true, "format": "percentage", "agg_rule": "average", "time_summary": "average"}),
		{"tool": "create_grid", "description": "Activities", "params": map[string]any{"name": "Activities", "dimensions": []string{"activity"}, "metrics": []string{"increment", "rate"}}},
		{"tool": "write_input_values", "description": "Increments", "params": map[string]any{"metric_id": "increment", "values": []map[string]any{
			{"members": map[string]string{"activity": "A1"}, "value": 0.3}, {"members": map[string]string{"activity": "A2"}, "value": 2}}}},
		{"tool": "write_input_values", "description": "Rates", "params": map[string]any{"metric_id": "rate", "values": []map[string]any{
			{"members": map[string]string{"activity": "A1"}, "value": 5.6}}}},
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{proposeResp("c1", plan...), proposeResp("c2", plan...)}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build it"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s\ntool results: %q", status, sse, toolResults(t, env))
	}
	results := toolResults(t, env)
	if len(results) < 1 {
		t.Fatal("no tool result")
	}
	if !strings.Contains(results[0], "increment shows no decimal places") || !strings.Contains(results[0], "0.3 would show as 0") {
		t.Errorf("no warning for increment:\n%s", results[0])
	}
	if strings.Contains(results[0], "rate shows no decimal places") {
		t.Errorf("rate keeps the default two decimals, yet was warned of:\n%s", results[0])
	}
}

// The counter under the title follows every call of a turn, not only its end
// (2026-11-25: it stood still through a turn while the session list moved on).
func TestSendMessage_SendsUsageAfterEveryCall(t *testing.T) {
	u := providers.Usage{InputTokens: 1000, OutputTokens: 50}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		{FinishReason: "tool_calls", Usage: u, Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: "c1", Name: "get_model_summary", Arguments: json.RawMessage(`{}`)}}}},
		{FinishReason: "stop", Usage: u, Message: providers.Message{Role: "assistant", Content: "The model holds three metrics."}},
	}}
	env := newAITurnEnv(t, fake)
	status, sse := env.send("what does the model hold")
	if status != http.StatusOK {
		t.Fatalf("turn: %d\n%s", status, sse)
	}
	first := strings.Index(sse, "event: usage\ndata: {\"usage\":{\"llm_calls\":1,\"input_tokens\":1000,")
	second := strings.Index(sse, "event: usage\ndata: {\"usage\":{\"llm_calls\":2,\"input_tokens\":2000,")
	done := strings.Index(sse, "event: done\n")
	if first < 0 || second < first || done < second {
		t.Errorf("want a usage event after each call, before done (at %d, %d, %d):\n%s", first, second, done, sse)
	}
}

// A turn that ends in a plan reports the session's usage with it, as a turn
// that ends in a reply does (2026-11-25: the counter stood still through four
// turns that each ended in a plan).
func TestSendMessage_PlanCarriesTheUsage(t *testing.T) {
	plan := proposeResp("c1", turnMetricStep("revenue", map[string]any{"is_input": true}))
	plan.Usage = providers.Usage{InputTokens: 1200, OutputTokens: 80}
	env := newAITurnEnv(t, &multiScriptProvider{resps: []providers.ChatResponse{plan}})
	status, sse := env.send("add a revenue input")
	at := strings.Index(sse, "event: proposal\n")
	if status != http.StatusOK || at < 0 {
		t.Fatalf("send: %d\n%s", status, sse)
	}
	frame := sse[at:]
	if end := strings.Index(frame, "\n\n"); end > 0 {
		frame = frame[:end]
	}
	if !strings.Contains(frame, `"usage":{"llm_calls":1,"input_tokens":1200,"output_tokens":80,`) || !strings.Contains(frame, `"memory":`) {
		t.Errorf("the proposal frame carries no usage or memory:\n%s", frame)
	}
}

// A session keeps what its calls were billed for, as the provider reports
// it, and AI_SESSION_TOKEN_LIMIT stops it at a budget (2026-11-07: a build's
// cost was only in the gateway log).
func TestSendMessage_RecordsTokenUsageAndStopsAtTheBudget(t *testing.T) {
	reply := func(text string) providers.ChatResponse {
		return providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: text},
			Usage: providers.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 500}}
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{reply("The grid holds 3 metrics."), reply("It has no time dimension.")}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("what does the grid hold"); status != http.StatusOK || !strings.Contains(sse, `"usage":{"llm_calls":1,"input_tokens":1000,"output_tokens":200,"cache_read_tokens":500`) {
		t.Fatalf("first turn: %d\n%s", status, sse)
	}
	u, err := env.chat.Usage(context.Background(), env.session)
	if err != nil || u.Calls != 1 || u.Total() != 1700 {
		t.Fatalf("usage after one call = %+v (%v), want 1 call and 1700 tokens", u, err)
	}
	t.Setenv("AI_SESSION_TOKEN_LIMIT", "1500")
	if status, body := env.send("and its dimensions?"); status != http.StatusTooManyRequests || !strings.Contains(body, "limit of 1500 per session") ||
		!strings.Contains(body, "whoever runs this server") {
		t.Errorf("a turn past the budget: %d %s, want 429 naming the limit and who sets it", status, body)
	}
}

// The daily call limit is the developer's own (AI Developer settings) or
// their tenant admin's (AI keys, enterprise), the lower of the two, else the
// server's default; the refusal names who set it (owner, 2026-11-16).
func TestSendMessage_DailyLimitByWhoSetIt(t *testing.T) {
	reply := providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Done."}}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{reply, reply, reply, reply}}
	enterprise := license.Static(&license.Claims{Edition: license.EditionEnterprise, Customer: "TurnCo", ExpiresAt: time.Now().Add(time.Hour)})
	env := newAITurnEnv(t, fake, func(h *handler) { h.lic = enterprise })
	ctx := context.Background()
	if status, sse := env.send("what does the grid hold"); status != http.StatusOK {
		t.Fatalf("first turn: %d\n%s", status, sse)
	}
	settings := func() map[string]any {
		t.Helper()
		status, body := env.get("/api/ai/settings")
		var out map[string]any
		if status != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("GET settings: %d %s", status, body)
		}
		return out
	}
	put := func(body string) {
		t.Helper()
		if status, out := env.do(http.MethodPut, "/api/ai/settings", []byte(body)); status != http.StatusOK {
			t.Fatalf("PUT settings %s: %d %s", body, status, out)
		}
	}

	// The developer's own limit, below the day's one call.
	put(`{"provider":"openai","daily_call_limit":1}`)
	if s := settings(); s["daily_call_limit"] != float64(1) || s["daily_limit"] != float64(1) || s["daily_limit_by"] != "own" {
		t.Fatalf("settings after own limit: %+v", s)
	}
	if status, body := env.send("and its dimensions?"); status != http.StatusTooManyRequests || !strings.Contains(body, "your own limit of 1 model calls a day, set in AI Developer settings") {
		t.Fatalf("own limit: %d %s", status, body)
	}
	// A save that leaves the limit out keeps it; a bad one is refused.
	put(`{"provider":"openai"}`)
	if s := settings(); s["daily_call_limit"] != float64(1) {
		t.Fatalf("limit not kept: %+v", s)
	}
	if status, _ := env.do(http.MethodPut, "/api/ai/settings", []byte(`{"provider":"openai","daily_call_limit":-2}`)); status != http.StatusBadRequest {
		t.Fatalf("negative limit: %d, want 400", status)
	}

	// The tenant admin's limit, lower than the developer's own.
	put(`{"provider":"openai","daily_call_limit":5000}`)
	var custID string
	if err := env.db.QueryRow(ctx, `SELECT id::text FROM core.customer WHERE name = 'TurnCo'`).Scan(&custID); err != nil {
		t.Fatal(err)
	}
	one := 1
	if err := aikeys.NewStore(env.db).SetDailyCallLimit(ctx, custID, &one); err != nil {
		t.Fatal(err)
	}
	if s := settings(); s["tenant_daily_call_limit"] != float64(1) || s["daily_limit_by"] != "tenant" {
		t.Fatalf("settings after tenant limit: %+v", s)
	}
	if status, body := env.send("and its dimensions?"); status != http.StatusTooManyRequests || !strings.Contains(body, "your tenant's limit of 1 model calls a day") ||
		!strings.Contains(body, "a tenant admin changes it under AI keys") {
		t.Fatalf("tenant limit: %d %s", status, body)
	}

	// Neither: the server's default, which the developer can go above.
	if err := aikeys.NewStore(env.db).SetDailyCallLimit(ctx, custID, nil); err != nil {
		t.Fatal(err)
	}
	put(`{"provider":"openai","daily_call_limit":null}`)
	t.Setenv("AI_MAX_CALLS_PER_DAY", "1")
	if status, body := env.send("and its dimensions?"); status != http.StatusTooManyRequests || !strings.Contains(body, "this server's default limit of 1 model calls a day") ||
		!strings.Contains(body, "set a limit of your own in AI Developer settings") {
		t.Fatalf("server default: %d %s", status, body)
	}
	put(`{"provider":"openai","daily_call_limit":10}`)
	if status, sse := env.send("and its dimensions?"); status != http.StatusOK || strings.Contains(sse, "limit of") {
		t.Fatalf("own limit above the server's default: %d\n%s", status, sse)
	}
}

// A step failing only over what a failed step was to make is told so in
// one line, naming that step: one wrong formula used to bring a dozen long
// errors, and the model chased those (2026-11-07).
func TestSendMessage_RefusalFoldsWhatFailsOnlyBecauseOfAnother(t *testing.T) {
	root := turnMetricStep("gross", map[string]any{"formula": "no_such_metric * 2"})
	follower := turnMetricStep("margin", map[string]any{"formula": "gross / 2"})
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		proposeResp("c1", root, follower),
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "gross fails: no_such_metric does not exist."}},
	}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build the margin"); status != http.StatusOK {
		t.Fatalf("send: %d\n%s", status, sse)
	}
	results := toolResults(t, env)
	if len(results) == 0 {
		t.Fatal("no tool result")
	}
	got := results[0]
	if !strings.Contains(got, "step 1 (create_metric") || !strings.Contains(got, "no_such_metric") {
		t.Errorf("the root failure is not reported in full:\n%s", got)
	}
	if !strings.Contains(got, `step 2 (create_metric — Create margin): uses "gross", which step 1 was to make and fails — fix step 1`) {
		t.Errorf("the follower is not folded into one line naming step 1:\n%s", got)
	}
}

// A reply calling an attached sheet done goes back once while the session's
// comparisons do not show it matching (2026-11-07: "the five grids are now
// populated" with Actual_PnL at 10 of 36 rows).
func TestSendMessage_SheetCalledDoneIsCheckedFirst(t *testing.T) {
	wb := excelize.NewFile()
	_ = wb.SetSheetName("Sheet1", "Actual_PnL")
	var buf bytes.Buffer
	if err := wb.Write(&buf); err != nil {
		t.Fatal(err)
	}
	stop := func(text string) providers.ChatResponse {
		return providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: text}}
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		stop("Actual_PnL is complete: every row reproduces the workbook."),
		stop("Actual_PnL is not done: I have not compared its rows yet."),
	}}
	env := newAITurnEnv(t, fake)
	if _, err := env.docs.CreateDocument(context.Background(), env.session, "model.xlsx",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "## Sheet: Actual_PnL", false, buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	status, sse := env.send("finish Actual_PnL")
	if status != http.StatusOK || !strings.Contains(sse, "I have not compared its rows yet") {
		t.Fatalf("the claim was not sent back: %d\n%s", status, sse)
	}
	if fake.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (the claim, then the answer to the check)", fake.calls)
	}
	msgs, _ := env.chat.ListMessages(context.Background(), env.session)
	if len(msgs) < 2 || !strings.Contains(msgs[len(msgs)-1].Content, "not done") {
		t.Errorf("the turn does not end on the checked answer: %+v", msgs[len(msgs)-1])
	}
}

// A failing layout step does not hold the plan back: the server leaves it
// out and says so (2026-11-11: a 38-step plan went back five times over one
// reorder_grid_metrics naming a metric not on the grid).
func TestSendMessage_FailingLayoutStepIsLeftOut(t *testing.T) {
	plan := []map[string]any{
		turnMetricStep("units", map[string]any{"is_input": true}),
		{"tool": "create_grid", "description": "Sales", "params": map[string]any{"name": "Sales", "metrics": []string{"units"}}},
		{"tool": "reorder_grid_metrics", "description": "Order Sales", "params": map[string]any{"grid_id": "Sales", "metrics": []string{"units", "not_on_the_grid"}}},
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{proposeResp("c1", plan...)}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build it"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s\ntool results: %q", status, sse, toolResults(t, env))
	}
	results := toolResults(t, env)
	if len(results) != 1 || !strings.Contains(results[0], "Left out by the server: step 3 (reorder_grid_metrics") {
		t.Fatalf("tool results: %q", results)
	}
	props, err := env.proposals.ListProposals(context.Background(), env.session)
	if err != nil || len(props) != 1 || len(props[0].Steps) != 2 {
		t.Fatalf("proposals: %v %+v", err, props)
	}
}
