package gateway

// The turn reads the developer's message before working on it (owner,
// 2026-11-08): a direct order, a delegated goal or a question, and which
// tasks it is about (aiassistant/task_mode.go). The turn's instructions then
// carry that mode, the playbook of each task and the session's task plan;
// plan_task and update_task_plan write the plan (aiassistant/task_plan.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// classifyRequest reads msg: a short "confirmed, continue" while a plan has
// stages left goes on with the plan; otherwise one call of the session's
// model, and the rules when that cannot be made.
func classifyRequest(ctx context.Context, p providers.Provider, model, msg string, plan *aiassistant.TaskPlan, store *aiassistant.ChatStore, sessionID string) aiassistant.RequestMode {
	rules := aiassistant.ClassifyByRules(msg, plan)
	if p == nil || (rules.Mode == aiassistant.ModeDelegated && plan.Current() >= 0 && len(rules.Tasks) == 0) {
		return rules
	}
	content := msg
	if cur := plan.Current(); cur >= 0 {
		content = fmt.Sprintf("(A task plan is in progress; its current stage: %s.)\n\n%s", plan.Stages[cur].Title, msg)
	}
	// A reading must not hold the turn up: the rules answer after 30s.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := p.Chat(cctx, providers.ChatRequest{Model: model, SystemPrompt: aiassistant.ClassifierPrompt,
		Messages: []providers.Message{{Role: "user", Content: content}}})
	if err != nil {
		return rules
	}
	if u := resp.Usage; u.InputTokens+u.CacheReadTokens > 0 && store != nil {
		_ = store.AddUsage(context.WithoutCancel(ctx), sessionID, u)
	}
	m, ok := aiassistant.ParseClassification(resp.Message.Content)
	if !ok {
		return rules
	}
	return m
}

// modeStatus is what the developer is shown the message was read as.
func modeStatus(m aiassistant.RequestMode) string {
	switch m.Mode {
	case aiassistant.ModeDirect:
		return "read as a direct order: doing exactly what it names"
	case aiassistant.ModeQuestion:
		return "read as a question"
	}
	return "read as a delegated goal: planning, building and checking it"
}

// runTool runs a tool the model calls outside a proposal: the task plan
// tools here, every other through exec.
func runTool(ctx context.Context, store *aiassistant.ChatStore, sessionID string, exec *aiassistant.ToolExecutor, name string, args json.RawMessage) (string, error) {
	switch name {
	case aiassistant.PlanTaskTool:
		var a struct {
			Goal   string                  `json:"goal"`
			Stages []aiassistant.TaskStage `json:"stages"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("plan_task: %w", err)
		}
		plan, err := aiassistant.NewTaskPlan(a.Goal, a.Stages)
		if err != nil {
			return "", err
		}
		if err := store.SetTaskPlan(ctx, sessionID, plan); err != nil {
			return "", err
		}
		log.Printf("AI task plan: session=%s stages=%d", sessionID, len(plan.Stages))
		return "Plan saved; the developer sees it.\n" + plan.Render() +
			"Now propose stage 1 (mark it in_progress with update_task_plan as you do), and say in your reply what the stages are.", nil
	case aiassistant.UpdateTaskPlanTool:
		var a struct {
			Stage  int    `json:"stage"`
			Status string `json:"status"`
			Note   string `json:"note"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("update_task_plan: %w", err)
		}
		plan, err := store.TaskPlan(ctx, sessionID)
		if err != nil {
			return "", err
		}
		if plan == nil {
			return "", fmt.Errorf("update_task_plan: the session has no plan — plan_task first")
		}
		if err := plan.UpdateStage(a.Stage, a.Status, a.Note); err != nil {
			return "", err
		}
		if err := store.SetTaskPlan(ctx, sessionID, plan); err != nil {
			return "", err
		}
		return plan.Render(), nil
	}
	return exec.Execute(ctx, name, args)
}

// isTaskPlanTool reports whether name writes the session's plan.
func isTaskPlanTool(name string) bool {
	return name == aiassistant.PlanTaskTool || name == aiassistant.UpdateTaskPlanTool
}
