package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// One agreed design, resolved once; done means verified (owner,
// 2026-11-12): update_target_model records the developer's choice (kept as
// a decision too) and the checks; the build's last stage closes only when
// every check is recorded, with the server's comparison count in its
// evidence.
func TestTargetDesignThroughTools(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()
	chat := aiassistant.NewChatStore(f.pool)
	status, body := f.do(t, "POST", "/api/ai/sessions", f.devSub, nil)
	var s struct {
		ID string `json:"id"`
	}
	if status != 200 || json.Unmarshal(body, &s) != nil {
		t.Fatalf("session: %d %s", status, body)
	}
	hooks := taskHooks{
		Sheets: func() []string { return []string{"Hiring Plan", "Headcount", "Import BS"} },
		Compared: func(sheets []string) string {
			return strings.Join(sheets, ", ") + " 18/18 rows, 648/648 periods matching"
		},
	}
	call := func(name string, args any) (string, error) {
		raw, _ := json.Marshal(args)
		return runTool(ctx, chat, s.ID, hooks, nil, name, raw)
	}
	target := map[string]any{"summary": "Startup FP&A.",
		"conventions": map[string]any{"units": "EUR", "signs": "costs positive", "percentages": "percent units", "opening_balances": "last closing",
			"scenarios": "Base in the saved workbook", "aggregation": "flows sum"},
		"sheets": []any{
			map[string]any{"sheet": "Hiring Plan", "role": "input", "grid": "Hiring"},
			map[string]any{"sheet": "Headcount", "role": "calculation", "grid": "Headcount", "metrics": []string{"fte"}},
			map[string]any{"sheet": "Import BS", "role": "actuals", "grid": "Balances"},
		},
		"decisions": []any{map[string]any{"question": "Do imported closing balances start the next forecast?", "options": []string{"no", "yes"}, "sheets": []string{"Import BS"}}},
		"checks":    []any{map[string]any{"name": "Funding disabled", "how": "switch funding off, compare cash"}},
	}
	out, err := call(aiassistant.SetTargetModelTool, target)
	if err != nil || !strings.Contains(out, "Ask the developer the open decisions now") {
		t.Fatalf("target: %v\n%s", err, out)
	}
	if _, err := call(aiassistant.PlanTaskTool, map[string]any{"goal": "Build", "stages": []any{
		map[string]any{"title": "Hiring Plan → Headcount", "items": []string{"hiring loaded", "FTE"}},
		map[string]any{"title": "Verification checks", "items": []string{"funding disabled"}},
	}}); err != nil {
		t.Fatal(err)
	}
	plan, _ := chat.TaskPlan(ctx, s.ID)
	if got := strings.Join(plan.Stages[0].StageSheets(), "+"); got != "Hiring Plan+Headcount" {
		t.Fatalf("module stage sheets = %q", got)
	}
	if plan.Target.HeldBack("Import BS") == "" {
		t.Fatal("Import BS not held back by its open decision")
	}
	if _, err := call(aiassistant.UpdateTargetModelTool, map[string]any{"decisions": []any{map[string]any{"question": "do imported closing balances start the next forecast?", "choice": "yes"}}}); err != nil {
		t.Fatal(err)
	}
	plan, _ = chat.TaskPlan(ctx, s.ID)
	if plan.Target.HeldBack("Import BS") != "" || len(plan.Decisions) != 1 || !strings.Contains(plan.Decisions[0], "→ yes") {
		t.Fatalf("decision not recorded: %+v / %v", plan.Target.Decisions, plan.Decisions)
	}
	ev := map[string]any{"built": "hiring and headcount", "differences": "none", "next": "Verification checks"}
	if _, err := call(aiassistant.UpdateTaskPlanTool, map[string]any{"stage": 1, "status": "done", "evidence": ev}); err != nil {
		t.Fatal(err)
	}
	plan, _ = chat.TaskPlan(ctx, s.ID)
	if e := plan.Stages[0].Evidence; plan.Stages[0].Status != aiassistant.StageDone || e == nil || !strings.Contains(e.Compared, "Hiring Plan, Headcount 18/18 rows") {
		t.Fatalf("done stage: %+v %+v", plan.Stages[0], e)
	}
	last := map[string]any{"built": "checks recorded", "differences": "none", "next": "none"}
	if _, err := call(aiassistant.UpdateTaskPlanTool, map[string]any{"stage": 2, "status": "done", "evidence": last}); err == nil || !strings.Contains(err.Error(), "Funding disabled") {
		t.Fatalf("the last stage closed with a check open: %v", err)
	}
	if _, err := call(aiassistant.UpdateTargetModelTool, map[string]any{"checks": []any{map[string]any{"name": "Funding disabled", "status": "passed", "evidence": "cash 36/36"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(aiassistant.UpdateTaskPlanTool, map[string]any{"stage": 2, "status": "done", "evidence": last}); err != nil {
		t.Fatal(err)
	}
	plan, _ = chat.TaskPlan(ctx, s.ID)
	if !plan.Finished() {
		t.Fatalf("plan not finished: %+v", plan.Stages)
	}
}

// A developer message that only confirms or continues is not a correction.
func TestIsCorrection(t *testing.T) {
	for msg, want := range map[string]bool{
		"Confirmed. Continue with the next stage of the plan.": false,
		"Go ahead.": false,
		"ok":        false,
		"Continue the task plan from where the previous session stopped.": false,
		"Fraction: the workbook stores 0.05 and shows it as 5%.":          true,
		"Use Product as a dimension instead.":                             true,
		"Continue? Why is P&L row 12 off?":                                true,
	} {
		if got := isCorrection(msg); got != want {
			t.Errorf("isCorrection(%q) = %v, want %v", msg, got, want)
		}
	}
}

// Typing values the attached workbook holds draws a warning (load them
// instead); sample values asked for, and zeros, do not.
func TestTypedWorkbookValueWarnings(t *testing.T) {
	numbers := map[float64]bool{49: true, 149: true, 0.05: true, 400: true}
	steps := []aiassistant.ProposalStep{
		{Tool: "write_input_values", Params: json.RawMessage(`{"metric_id":"price_per_unit","values":[{"members":{"product":"STARTER"},"value":49},{"members":{"product":"PRO"},"value":149}]}`)},
		{Tool: "write_input_values", Params: json.RawMessage(`{"metric_id":"growth","values":[{"members":{"product":"STARTER"},"value":5},{"members":{"product":"PRO"},"value":0}]}`)},
		{Tool: "write_input_values", Params: json.RawMessage(`{"metric_id":"other","values":[{"members":{"x":"A"},"value":7},{"members":{"x":"B"},"value":8}]}`)},
	}
	w := typedWorkbookValueWarnings(steps, "Continue.", numbers)
	if len(w) != 1 || !strings.Contains(w[0], "step 1 (write_input_values price_per_unit) types 2 of 2") {
		t.Fatalf("warnings: %v", w)
	}
	if w := typedWorkbookValueWarnings(steps, "Fill the grid with sample values", numbers); len(w) != 0 {
		t.Errorf("sample values asked for, still warned: %v", w)
	}
}
