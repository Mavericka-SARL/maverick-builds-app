package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// A plan that does not arrive whole — cut off at the output limit, or not
// JSON — goes back to the model with why and what to do, and the corrected
// plan reaches the developer; the turn no longer ends on "invalid steps".
func TestSendMessage_UnreadablePlanGoesBackToTheModel(t *testing.T) {
	cut := providers.ChatResponse{FinishReason: "length", Message: providers.Message{Role: "assistant",
		ToolCalls: []providers.ToolCall{{ID: "c1", Name: "propose_actions", Arguments: []byte(`{"steps":[{"tool":"create_metric","params":{"name":"a`)}}}}
	broken := providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
		ToolCalls: []providers.ToolCall{{ID: "c2", Name: "propose_actions", Arguments: []byte(`{"steps":[{"tool":"create_metric"`)}}}}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{cut, broken,
		proposeResp("c3", turnMetricStep("revenue", map[string]any{"is_input": true}))}}
	env := newAITurnEnv(t, fake)
	status, sse := env.send("add revenue")
	if status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("no proposal after the plan was sent back: %d\n%s\n%q", status, sse, toolResults(t, env))
	}
	results := toolResults(t, env)
	if len(results) < 3 || !strings.Contains(results[0], "cut off at your output limit") || !strings.Contains(results[1], "not complete JSON") {
		t.Errorf("tool results = %q", results)
	}
}

// A third plan that does not arrive ends the turn with a message saying so.
func TestSendMessage_UnreadablePlanStopsAfterTwo(t *testing.T) {
	broken := func(args string) providers.ChatResponse {
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: "c", Name: "propose_actions", Arguments: []byte(args)}}}}
	}
	env := newAITurnEnv(t, &multiScriptProvider{resps: []providers.ChatResponse{
		broken(`{"steps":[]}`), broken(`{"steps":[{"tool"`), broken(`{"steps":`)}})
	_, sse := env.send("add revenue")
	if !strings.Contains(sse, "event: error") || !strings.Contains(sse, "did not arrive whole") {
		t.Fatalf("the turn did not stop with a message:\n%s", sse)
	}
	if n := len(toolResults(t, env)); n != 2 {
		t.Errorf("%d plans went back, want 2", n)
	}
}

// An empty reply is sent back once; the developer sees the next one and no
// empty message.
func TestSendMessage_EmptyReplyIsSentBackOnce(t *testing.T) {
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "  "}},
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Revenue is on the Sales grid; nothing to change."}},
	}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("where is revenue?"); status != http.StatusOK || !strings.Contains(sse, "event: done") {
		t.Fatalf("send: %d\n%s", status, sse)
	}
	msgs, _ := env.chat.ListMessages(context.Background(), env.session)
	for _, m := range msgs {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 {
			t.Errorf("an empty reply was saved")
		}
	}
	if last := msgs[len(msgs)-1]; !strings.Contains(last.Content, "nothing to change") {
		t.Errorf("last message = %q", last.Content)
	}
}

// A new proposal withdraws the pending one it replaces, and both the model
// and the developer are told.
func TestSendMessage_NewProposalWithdrawsThePendingOne(t *testing.T) {
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		proposeResp("c1", turnMetricStep("revenue", map[string]any{"is_input": true}), turnMetricStep("cost", map[string]any{"is_input": true})),
		proposeResp("c2", turnMetricStep("revenue", map[string]any{"is_input": true})),
	}}
	env := newAITurnEnv(t, fake)
	env.send("add revenue and cost")
	env.send("only revenue")
	ps, err := env.proposals.ListProposals(context.Background(), env.session)
	if err != nil || len(ps) != 2 {
		t.Fatalf("proposals: %d (%v)", len(ps), err)
	}
	statuses := map[string]int{}
	for _, p := range ps {
		statuses[p.Status]++
	}
	if statuses["pending"] != 1 || statuses["rejected"] != 1 {
		t.Errorf("statuses = %v, want one pending and one withdrawn", statuses)
	}
	results := toolResults(t, env)
	if !strings.Contains(results[len(results)-1], "replaces the earlier pending proposal (2 step(s))") {
		t.Errorf("the model was not told: %q", results[len(results)-1])
	}
	msgs, _ := env.chat.ListMessages(context.Background(), env.session)
	told := false
	for _, m := range msgs {
		told = told || (m.Role == "assistant" && strings.Contains(m.Content, "replaces the earlier one (2 step(s))"))
	}
	if !told {
		t.Error("the developer was not told")
	}
}

// The plan check's warnings for the Creator & Agency build's mistakes: an
// input moved that the request does not name, a calculated row made an
// input, a property no member holds read by a formula, and a total by
// formula given a time summary it never applies.
func TestSendMessage_WarnsOfMovedInputsAndSilentFormulas(t *testing.T) {
	setup := []map[string]any{
		{"tool": "create_dimension", "description": "Months", "params": map[string]any{"name": "period", "dimension_type": "time",
			"time_granularity": "month", "fiscal_year_start_month": 1, "members": []map[string]any{{"code": "FY27", "label": "FY27"}}}},
		{"tool": "generate_time_members", "description": "Months", "params": map[string]any{"dimension_id": "period", "start": "2027-01-01", "end": "2027-03-31", "parent_code": "FY27"}},
		{"tool": "create_dimension", "description": "Accounts", "params": map[string]any{"name": "acct",
			"members": []map[string]any{{"code": "A1", "label": "A1"}, {"code": "A2", "label": "A2"}}}},
		{"tool": "add_dimension_property", "description": "Line", "params": map[string]any{"dimension_id": "acct", "name": "line", "data_type": "text"}},
		turnMetricStep("price", map[string]any{"is_input": true}),
		turnMetricStep("cost", map[string]any{"formula": "price * 2"}),
		{"tool": "create_grid", "description": "Prices", "params": map[string]any{"name": "Prices", "dimensions": []string{"acct"}, "metrics": []string{"price", "cost"}}},
		{"tool": "write_input_values", "description": "Prices", "params": map[string]any{"metric_id": "price", "values": []map[string]any{
			{"members": map[string]string{"acct": "A1"}, "value": 10}, {"members": map[string]string{"acct": "A2"}, "value": 20}}}},
		turnMetricStep("util", map[string]any{"is_input": true, "format": "percentage", "agg_rule": "average", "time_summary": "average"}),
		{"tool": "create_grid", "description": "Monthly", "params": map[string]any{"name": "Monthly", "dimensions": []string{"period"}, "metrics": []string{"util"}}},
	}
	change := []map[string]any{
		{"tool": "write_input_values", "description": "Price", "params": map[string]any{"metric_id": "price", "values": []map[string]any{
			{"members": map[string]string{"acct": "A1"}, "value": 13.5}}}},
		{"tool": "update_metric", "description": "Cost as input", "params": map[string]any{"metric_id": "cost", "is_input": true}},
		turnMetricStep("line_x", map[string]any{"formula": `SUMIFS(price, acct.line, "X")`}),
		{"tool": "add_grid_metric", "description": "Line X on Prices", "params": map[string]any{"grid_id": "Prices", "metric_id": "line_x"}},
		turnMetricStep("peak_util", map[string]any{"formula": "util", "format": "percentage", "agg_rule": "formula", "time_summary": "max"}),
		{"tool": "add_grid_metric", "description": "Peak on Monthly", "params": map[string]any{"grid_id": "Monthly", "metric_id": "peak_util"}},
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{proposeResp("c1", setup...), proposeResp("c2", change...), proposeResp("c3", change...)}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("set up the prices"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("setup: %d\n%s\n%q", status, sse, toolResults(t, env))
	}
	ps, _ := env.proposals.ListProposals(context.Background(), env.session)
	if status, body := env.confirm(ps[0].ID); status != http.StatusOK || !strings.Contains(body, `"executed"`) {
		t.Fatalf("confirm: %d %s", status, body)
	}
	if status, sse := env.send("make the totals match the workbook"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("change: %d\n%s\n%q", status, sse, toolResults(t, env))
	}
	results := toolResults(t, env)
	sentBack := results[len(results)-2]
	for _, w := range []string{
		"write_input_values price): changes 1 value(s) the input already holds (A1: 10 → 13.5)",
		"turns the calculated metric cost into an input, dropping its formula price * 2",
		"reads acct.line, which none of acct's 2 members has a value for",
		`peak_util totals by its formula (agg_rule "formula"), so its time_summary "max" never applies`,
	} {
		if !strings.Contains(sentBack, w) {
			t.Errorf("the warnings lack %q:\n%s", w, sentBack)
		}
	}
	if !strings.Contains(results[len(results)-1], "Shown with the plan check's warnings") {
		t.Errorf("the plan proposed again unchanged is not shown with its warnings: %q", results[len(results)-1])
	}
}

// "Add X to the formula" answered by a formula of X alone is refused: the
// High-Tech build's hc_new_fte_joined lost every FTE that way (2026-11-11).
// The corrected plan, keeping the formula and adding the term, goes through.
func TestSendMessage_RefusesAFormulaReplacedWhenAskedToAdd(t *testing.T) {
	setup := []map[string]any{
		turnMetricStep("hires", map[string]any{"is_input": true}),
		turnMetricStep("joined", map[string]any{"formula": "hires * 1"}),
	}
	replaced := []map[string]any{
		turnMetricStep("approved", map[string]any{"is_input": true}),
		{"tool": "update_metric", "description": "Joined reads approved", "params": map[string]any{"metric_id": "joined", "formula": "approved"}},
	}
	extended := []map[string]any{
		turnMetricStep("approved", map[string]any{"is_input": true}),
		{"tool": "update_metric", "description": "Joined adds approved", "params": map[string]any{"metric_id": "joined", "formula": "hires * 1 + approved"}},
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{proposeResp("c1", setup...), proposeResp("c2", replaced...), proposeResp("c3", extended...)}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("set up hires"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("setup: %d\n%s", status, sse)
	}
	ps, _ := env.proposals.ListProposals(context.Background(), env.session)
	if status, body := env.confirm(ps[0].ID); status != http.StatusOK || !strings.Contains(body, `"executed"`) {
		t.Fatalf("confirm: %d %s", status, body)
	}
	if status, sse := env.send("add the approved requests to the joined formula"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("change: %d\n%s\n%q", status, sse, toolResults(t, env))
	}
	results := toolResults(t, env)
	if len(results) < 2 || !strings.Contains(results[len(results)-2], "update_metric joined): the new formula drops everything the formula reads now (hires)") {
		t.Fatalf("the replacement was not refused: %q", results)
	}
}

// Two formulas that run and read wrong: a dimension property with an input
// metric's name (the scenario's values never read), and a total over a
// dimension the metric totals by none (no value at all).
func TestSendMessage_WarnsOfShadowedPropertyAndNoneTotal(t *testing.T) {
	plan := []map[string]any{
		{"tool": "create_dimension", "description": "Functions", "params": map[string]any{"name": "fn",
			"members": []map[string]any{{"code": "ENG", "label": "ENG"}, {"code": "OPS", "label": "OPS"}}}},
		{"tool": "create_dimension", "description": "Scenarios", "params": map[string]any{"name": "scen",
			"members": []map[string]any{{"code": "Base", "label": "Base"}, {"code": "Down", "label": "Down"}}}},
		{"tool": "create_dimension", "description": "Accounts", "params": map[string]any{"name": "acct",
			"members": []map[string]any{{"code": "X", "label": "X"}, {"code": "Y", "label": "Y"}}}},
		{"tool": "add_dimension_property", "description": "Salary", "params": map[string]any{"dimension_id": "fn", "name": "salary", "data_type": "number"}},
		turnMetricStep("salary", map[string]any{"is_input": true}),
		turnMetricStep("pay", map[string]any{"formula": "fn.salary * 2"}),
		turnMetricStep("amt", map[string]any{"is_input": true}),
		turnMetricStep("flag", map[string]any{"formula": "IF(amt > 0, 1, 0)", "agg_rule": "none"}),
		turnMetricStep("chk", map[string]any{"formula": "flag"}),
		{"tool": "create_grid", "description": "Salaries", "params": map[string]any{"name": "Salaries", "dimensions": []string{"fn", "scen"}, "metrics": []string{"salary"}}},
		{"tool": "create_grid", "description": "Pay", "params": map[string]any{"name": "Pay", "dimensions": []string{"fn"}, "metrics": []string{"pay"}}},
		{"tool": "create_grid", "description": "Flags", "params": map[string]any{"name": "Flags", "dimensions": []string{"acct"}, "metrics": []string{"amt", "flag"}}},
		{"tool": "create_grid", "description": "Checks", "params": map[string]any{"name": "Checks", "metrics": []string{"chk"}}},
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{proposeResp("c1", plan...), proposeResp("c2", plan...)}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build pay and checks"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s\n%q", status, sse, toolResults(t, env))
	}
	results := toolResults(t, env)
	for _, w := range []string{
		"reads the property fn.salary, and an input metric salary holds values by fn × scen",
		"reads flag over acct, which this metric's grid does not have",
	} {
		if !strings.Contains(results[0], w) {
			t.Errorf("the warnings lack %q:\n%s", w, results[0])
		}
	}
}

// A table's approval workflow started on submit with no table subject is
// refused (the approved record never moved); a workflow condition over a
// summed Scenario, and a Scenario dimension that adds up, are warned of.
func TestSendMessage_RecordWorkflowNeedsItsSubjectAndScenarioTotals(t *testing.T) {
	steps := func(subject bool) []map[string]any {
		upd := map[string]any{"workflow_def_id": "<created in step 2>", "steps": []map[string]any{
			{"id": "ok", "name": "Passes?", "type": "condition", "condition": map[string]any{"formula": "pass_flag = 1"}, "routes": map[string]any{"true": "approve", "false": "approve"}},
			{"id": "approve", "name": "Approve", "type": "approval", "assignee_roles": []string{"Finance"}, "routes": map[string]any{"approve": "end", "reject": "end"}},
			{"id": "end", "name": "End", "type": "end"},
		}}
		if subject {
			upd["subject_type"] = "form_record"
			upd["subject_config"] = map[string]any{"form_id": "hire_request"}
		}
		return []map[string]any{
			{"tool": "create_form_def", "description": "Requests", "params": map[string]any{"name": "hire_request", "label": "Hire request",
				"fields": []map[string]any{{"name": "fte", "label": "FTE", "type": "number"}, {"name": "status", "label": "Status", "type": "status"}}}},
			{"tool": "create_workflow_def", "description": "Approval", "params": map[string]any{"name": "Hire approval", "trigger_event": "form_submit"}},
			{"tool": "update_workflow_def", "description": "Steps", "params": upd},
			{"tool": "create_automation_rule", "description": "On submit", "params": map[string]any{"name": "Hire submitted", "trigger_type": "form_submit", "source_form_id": "hire_request", "workflow_name": "Hire approval"}},
			{"tool": "create_dimension", "description": "Scenarios", "params": map[string]any{"name": "Scenario",
				"members": []map[string]any{{"code": "Base", "label": "Base"}, {"code": "Down", "label": "Down"}}}},
			turnMetricStep("pass_flag", map[string]any{"is_input": true}),
			{"tool": "create_grid", "description": "Checks", "params": map[string]any{"name": "Checks", "dimensions": []string{"Scenario"}, "metrics": []string{"pass_flag"}}},
			{"tool": "create_business_role", "description": "Finance", "params": map[string]any{"name": "Finance"}},
		}
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{proposeResp("c1", steps(false)...), proposeResp("c2", steps(true)...), proposeResp("c3", steps(true)...)}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build the hiring approval"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s\n%q", status, sse, toolResults(t, env))
	}
	results := toolResults(t, env)
	if !strings.Contains(results[0], "workflow Hire approval is started when a record of the table hire_request is submitted and has an approval, but its subject is not that table's record") {
		t.Errorf("the subject-less approval was not refused:\n%s", results[0])
	}
	all := strings.Join(results, "\n")
	for _, w := range []string{
		`condition "Passes?": reads pass_flag over Scenario, which no context variable picks`,
		"step 5 (create_dimension Scenario): its members are alternatives",
	} {
		if !strings.Contains(all, w) {
			t.Errorf("no warning %q in:\n%s", w, all)
		}
	}
}
