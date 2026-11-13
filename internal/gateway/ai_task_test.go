package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// planningProvider reads every message as a delegated goal, plans it on its
// first call, then answers; it keeps the system prompt of the turn's calls.
type planningProvider struct {
	calls   int
	prompts []string
}

func (p *planningProvider) Chat(_ context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	if req.SystemPrompt == aiassistant.ClassifierPrompt {
		return providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant",
			Content: `{"mode": "delegated", "tasks": ["pages"]}`}}, nil
	}
	if r, ok := sideCall(req); ok {
		return r, nil
	}
	p.calls++
	p.prompts = append(p.prompts, req.SystemPrompt)
	if p.calls == 1 {
		args, _ := json.Marshal(map[string]any{"goal": "Add the pages people need", "stages": []map[string]any{
			{"title": "Demand & revenue", "items": []string{"page Demand & revenue"}, "check": "describe_dashboard"},
			{"title": "Variance", "items": []string{"page Variance"}},
		}})
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: "call_plan", Name: aiassistant.PlanTaskTool, Arguments: args}}}}, nil
	}
	return providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Planned: two stages."}}, nil
}

// TestADelegatedGoalIsPlannedAndThePlanCarriesOn: a message read as a
// delegated goal gets the delegated brief and the pages playbook; plan_task
// keeps the plan with the session (served with it, sent as it changes), and
// the next turn is told its current stage (owner, 2026-11-08).
func TestADelegatedGoalIsPlannedAndThePlanCarriesOn(t *testing.T) {
	fake := &planningProvider{}
	env := newAITurnEnv(t, fake)
	status, sse := env.send("Add the pages people need for every sheet of the workbook")
	if status != http.StatusOK || !strings.Contains(sse, "read as a delegated goal") || !strings.Contains(sse, `"task_plan"`) {
		t.Fatalf("turn: %d\n%.600s", status, sse)
	}
	if len(fake.prompts) == 0 || !strings.Contains(fake.prompts[0], "DELEGATED A GOAL") || !strings.Contains(fake.prompts[0], "### Playbook: dashboards") {
		t.Errorf("the turn's instructions lack the delegated brief or the pages playbook")
	}
	plan, err := env.chat.TaskPlan(context.Background(), env.session)
	if err != nil || plan == nil || len(plan.Stages) != 2 || plan.Current() != 0 {
		t.Fatalf("the session's plan: %+v %v", plan, err)
	}
	fake.prompts = nil
	if status, _ := env.send("Confirmed. Continue."); status != http.StatusOK {
		t.Fatalf("second turn: %d", status)
	}
	if len(fake.prompts) == 0 || !strings.Contains(fake.prompts[0], "1. [to do, CURRENT] Demand & revenue — items: page Demand & revenue") {
		t.Errorf("the next turn is not told its current stage:\n%.400s", strings.Join(fake.prompts, "\n"))
	}
}
