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
