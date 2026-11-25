package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// Think multidimensionally (owner, 2026-11-13): a plan creating
// actuals_cost_2027 goes back to the AI, which proposes one metric cost.
func TestFlatMetricNameSentBack(t *testing.T) {
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		proposeResp("c1", turnMetricStep("actuals_cost_2027", map[string]any{"is_input": true})),
		proposeResp("c2", turnMetricStep("cost", map[string]any{"is_input": true})),
	}}
	env := newAITurnEnv(t, fake)
	status, sse := env.send("build the cost model")
	if status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("turn: %d\n%s", status, sse)
	}
	results := strings.Join(toolResults(t, env), "\n")
	if !strings.Contains(results, "Think multidimensionally") || !strings.Contains(results, `ONE metric "cost"`) {
		t.Fatalf("the flat name was not sent back: %s", results)
	}
	props, _ := env.proposals.ListProposals(context.Background(), env.session)
	if len(props) != 1 || !strings.Contains(string(props[0].Steps[0].Params), `"cost"`) || strings.Contains(string(props[0].Steps[0].Params), "2027") {
		t.Fatalf("proposal shown: %+v", props)
	}
}

// After the developer confirms a plan the console goes on by itself
// ({continue: true}); the server writes the message from what ran. With
// nothing run there is nothing to go on from.
func TestContinueAfterConfirm(t *testing.T) {
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		proposeResp("c1", turnMetricStep("cost", map[string]any{"is_input": true})),
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Next: add price. Shall I?"}},
	}, classify: `{"mode": "direct", "tasks": ["workbook_build", "pages"]}`} // a build, as live
	env := newAITurnEnv(t, fake)
	ctx := context.Background()
	if status, body := env.post("/api/ai/sessions/"+env.session+"/messages", []byte(`{"continue":true}`)); status != http.StatusConflict {
		t.Fatalf("go on with nothing run: %d %s, want 409", status, body)
	}
	if status, sse := env.send("add a cost input"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("turn: %d %s", status, sse)
	}
	props, _ := env.proposals.ListProposals(ctx, env.session)
	if status, body := env.confirm(props[0].ID); status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, body)
	}
	status, sse := env.post("/api/ai/sessions/"+env.session+"/messages", []byte(`{"continue":true}`))
	if status != http.StatusOK || !strings.Contains(sse, "event: done") {
		t.Fatalf("go on: %d %s", status, sse)
	}
	msgs, _ := env.chat.ListMessages(ctx, env.session)
	var last string
	for _, m := range msgs {
		if m.Role == "user" {
			last = m.Content
		}
	}
	if !strings.HasPrefix(last, aiassistant.ContinuePrefix) || !strings.Contains(last, "all 1 steps ran") || !strings.Contains(last, "without waiting") {
		t.Errorf("the go-on message: %q", last)
	}
	// The go-on turn's check is narrowed to what the plan changed, in a brief
	// sent with the turn's message — not in the instructions, which the
	// provider caches only while they stay the same; the developer's line
	// stays short and the turn before had no such brief (2026-11-25).
	if n := len(fake.lastUser); n < 2 || strings.Contains(fake.lastUser[0], "THE PLAN THAT JUST RAN") {
		t.Fatalf("calls: %d, or the first turn already carried a check brief", n)
	}
	brief := "" // the go-on turn's message as its first call sent it
	for _, u := range fake.lastUser {
		if strings.HasPrefix(u, aiassistant.ContinuePrefix) {
			brief = u
			break
		}
	}
	if !strings.HasPrefix(brief, last) || !strings.Contains(brief, "THE PLAN THAT JUST RAN") ||
		!strings.Contains(brief, "1. "+props[0].Steps[0].Tool+" — ") || !strings.Contains(brief, "Check only what these steps changed") {
		t.Errorf("the go-on turn's message does not carry what ran:\n%s", brief)
	}
	if strings.Contains(fake.prompts[len(fake.prompts)-1], "THE PLAN THAT JUST RAN") {
		t.Error("the check brief went into the instructions, which then change every go-on turn")
	}
	if strings.Contains(last, "THE PLAN THAT JUST RAN") {
		t.Errorf("the check brief leaked into the developer's line: %q", last)
	}
	// The typed turn (a build) and the go-on turn offer the same tools: the
	// tool list opens the provider's cache, so a changed list re-sent the
	// whole prompt uncached (2026-11-25).
	if first, goOn := strings.Join(fake.tools[0], ","), strings.Join(fake.tools[len(fake.tools)-1], ","); first != goOn {
		t.Errorf("the tools changed between the turns:\ntyped: %s\ngo-on: %s", first, goOn)
	}
	// And the same instructions: the plan added a metric and the turn's mode
	// changed, which goes with the turn's message, not into them.
	if typed, goOn := fake.prompts[0], fake.prompts[len(fake.prompts)-1]; typed != goOn {
		i := 0
		for i < len(typed) && i < len(goOn) && typed[i] == goOn[i] {
			i++
		}
		t.Errorf("the instructions changed between the turns at %d of %d:\ntyped: %q\ngo-on: %q", i, len(typed),
			typed[max(0, i-80):min(len(typed), i+160)], goOn[max(0, i-80):min(len(goOn), i+160)])
	}
	if !strings.Contains(brief, "## Current application context") || strings.Contains(fake.prompts[0], "## Current application context") {
		t.Error("the application context is not with the turn's message, or still in the instructions")
	}
}

func planTaskCall(id string) providers.ChatResponse {
	args := []byte(`{"goal":"Build the cost model","stages":[{"title":"Structure","items":["dimensions"]},{"title":"Costs","items":["cost inputs"]}]}`)
	return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
		ToolCalls: []providers.ToolCall{{ID: id, Name: aiassistant.PlanTaskTool, Arguments: args}}}}
}

// The developer approves the whole plan before anything is built (owner,
// 2026-11-13): a proposal for a plan that waits is refused and the turn ends
// on the plan; Approve plan starts the build with stage 1.
func TestPlanAwaitsApprovalBeforeBuilding(t *testing.T) {
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		planTaskCall("p1"),
		proposeResp("c1", turnMetricStep("department", map[string]any{"is_input": true})),
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "The plan: 1. Structure 2. Costs. Shall I go ahead? Approve it or tell me what to change."}},
		proposeResp("c2", turnMetricStep("cost", map[string]any{"is_input": true})),
	}}
	env := newAITurnEnv(t, fake)
	ctx := context.Background()
	status, sse := env.send("build the cost model from the workbook")
	if status != http.StatusOK || strings.Contains(sse, "event: proposal") || !strings.Contains(sse, "event: done") {
		t.Fatalf("planning turn: %d — want it to end on the plan with no proposal\n%s", status, sse)
	}
	if props, _ := env.proposals.ListProposals(ctx, env.session); len(props) != 0 {
		t.Fatalf("a proposal was made for a plan not approved: %+v", props)
	}
	if !strings.Contains(strings.Join(toolResults(t, env), "\n"), "awaits their approval") {
		t.Error("the proposal was not refused for the approval")
	}
	plan, _ := env.chat.TaskPlan(ctx, env.session)
	if !plan.AwaitsApproval() {
		t.Fatalf("plan %+v, want it waiting for approval", plan)
	}
	if fake.calls != 3 {
		t.Errorf("model calls = %d, want 3 (no nudge to propose while the plan waits)", fake.calls)
	}

	if status, body := env.post("/api/ai/sessions/"+env.session+"/plan/approve", nil); status != http.StatusOK || !strings.Contains(body, "approved_at") {
		t.Fatalf("approve: %d %s", status, body)
	}
	status, sse = env.post("/api/ai/sessions/"+env.session+"/messages", []byte(`{"continue":true}`))
	if status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("after approval: %d %s", status, sse)
	}
	msgs, _ := env.chat.ListMessages(ctx, env.session)
	var last string
	for _, m := range msgs {
		if m.Role == "user" {
			last = m.Content
		}
	}
	if !strings.HasPrefix(last, aiassistant.ContinuePrefix+"Plan approved") || !strings.Contains(last, "stage 1 (Structure)") {
		t.Errorf("the go-on message after approval: %q", last)
	}
}

// A reply that approves the plan approves it too, and the turn builds.
func TestPlanApprovedByReply(t *testing.T) {
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		planTaskCall("p1"),
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "The plan: 1. Structure 2. Costs. Approve it?"}},
		proposeResp("c1", turnMetricStep("cost", map[string]any{"is_input": true})),
	}}
	env := newAITurnEnv(t, fake)
	if status, _ := env.send("build the cost model"); status != http.StatusOK {
		t.Fatal("planning turn")
	}
	status, sse := env.send("Approve")
	if status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("approving reply: %d %s", status, sse)
	}
	if plan, _ := env.chat.TaskPlan(context.Background(), env.session); plan.AwaitsApproval() || plan.ApprovedAt == nil {
		t.Errorf("plan after the approving reply: %+v", plan)
	}
}
