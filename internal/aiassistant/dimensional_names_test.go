package aiassistant

import (
	"encoding/json"
	"strings"
	"testing"
)

// Think multidimensionally (owner, 2026-11-13): a version or a period in a
// metric's name is a dimension's member; flags, controls and comparisons
// that name them are measures of their own.
func TestFlatNameParts(t *testing.T) {
	flat := map[string]string{ // name → the one metric it should be
		"actuals_cost_2027": "cost", "budget_revenue": "revenue", "revenue_budget": "revenue", "cost_forecast": "cost",
		"revenue_q1": "revenue", "salary_jan": "salary", "opex_fy2027": "opex", "headcount_2028": "headcount",
		"prior_year_revenue": "revenue", "revenue_2027_actual": "revenue", "marketing_budget": "marketing",
		"revenue_upside": "revenue", // a member of the model's Scenario dimension
	}
	members := map[string]bool{"upside": true, "base": true}
	for name, measure := range flat {
		f := FlatNameParts(name, members)
		if !f.Flat() || f.Measure != measure {
			t.Errorf("%s: flat=%v measure=%q, want %q", name, f.Flat(), f.Measure, measure)
		}
	}
	for _, name := range []string{"is_actual", "actual_through_month", "last_closed_month_number", "budget_variance", "actual_vs_budget",
		"forecast_accuracy", "revenue", "budget", "gross_margin_pct", "salary_per_fte", "hiring_plan", "customer_count", "base_salary"} {
		if f := FlatNameParts(name, members); f.Flat() {
			t.Errorf("%s read as flat: versions %v periods %v", name, f.Versions, f.Periods)
		}
	}
	if msg := FlatNameParts("actuals_cost_2027", nil).Message(); !strings.Contains(msg, `ONE metric "cost"`) || !strings.Contains(msg, "Version dimension") ||
		!strings.Contains(msg, "time dimension") {
		t.Errorf("message: %s", msg)
	}
}

func TestFlatMetricProblems(t *testing.T) {
	step := func(tool, name string) ProposalStep {
		p, _ := json.Marshal(map[string]string{"name": name})
		return ProposalStep{Tool: tool, Params: p}
	}
	dims := []NamedDimension{{Name: "region", Members: []string{"emea", "apac", "americas"}}, {Name: "month", Time: true, Members: []string{"2027_01"}}}
	calc, _ := json.Marshal(map[string]any{"name": "forecast_base", "formula": "FORECAST(revenue)"})
	steps := []ProposalStep{step("create_metric", "actuals_cost_2027"), step("create_metric", "revenue_emea"), step("create_dimension", "budget_2027"),
		step("create_metric", "headcount"), {Tool: "create_metric", Params: calc}}
	probs, warns, failed := FlatMetricProblems(steps, "build the cost model", dims, []string{"revenue_apac"})
	if len(probs) != 1 || len(failed) != 1 || failed[0] != 0 || !strings.Contains(probs[0], "step 1") {
		t.Errorf("problems %v failed %v, want step 1 alone", probs, failed)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "revenue_emea and revenue_apac") || !strings.Contains(warns[0], "region") {
		t.Errorf("warnings %v, want revenue_emea/revenue_apac by region", warns)
	}
	// A name the developer wrote is theirs.
	if probs, _, _ := FlatMetricProblems(steps[:1], "create metric actuals_cost_2027 as an input", dims, nil); len(probs) != 0 {
		t.Errorf("the developer's own name was sent back: %v", probs)
	}
}

func TestTargetFlatMetrics(t *testing.T) {
	tm := &TargetModel{Summary: "x", Sheets: []TargetSheet{{Sheet: "Inputs", Role: "input", Grid: "in", Metrics: []string{"revenue", "budget_revenue_2027"}},
		{Sheet: "P&L", Role: "calculation", Grid: "pl", Metrics: []string{"h1_revenue"}}}}
	if err := targetFlatMetrics(tm); err == nil || !strings.Contains(err.Error(), `budget_revenue_2027 (sheet Inputs) → one metric "revenue"`) ||
		strings.Contains(err.Error(), "h1_revenue") {
		t.Errorf("target: %v", err)
	}
	tm.Sheets[0].Metrics = []string{"revenue", "cost"}
	if err := targetFlatMetrics(tm); err != nil {
		t.Error(err)
	}
}

// After a confirmed plan the AI goes on by itself: failures first, then the
// current stage's next part, or the next step of the request.
func TestContinuationMessage(t *testing.T) {
	plan := &TaskPlan{Goal: "g", Stages: []TaskStage{{Title: "Structure", Status: StageDone}, {Title: "P&L", Status: StageInProgress}}}
	m := ContinuationMessage(4, 0, plan)
	if !strings.HasPrefix(m, ContinuePrefix) || !strings.Contains(m, "all 4 steps ran") || !strings.Contains(m, "2. P&L") || !strings.Contains(m, "without waiting") {
		t.Errorf("stage under way: %s", m)
	}
	if m := ContinuationMessage(4, 1, plan); !strings.Contains(m, "3 of 4 steps ran, 1 failed") || !strings.Contains(m, "first fix what failed") {
		t.Errorf("failures: %s", m)
	}
	if m := ContinuationMessage(2, 0, nil); !strings.Contains(m, "next step of my request") {
		t.Errorf("no plan: %s", m)
	}
	plan.Stages[1].Status = StageDone
	if m := ContinuationMessage(2, 0, plan); !strings.Contains(m, "stages are all done") && !strings.Contains(m, "report what was built") {
		t.Errorf("finished plan: %s", m)
	}
}

// A reply that approves the plan, as a whole message; a change request or
// a question does not.
func TestApprovesPlan(t *testing.T) {
	for _, m := range []string{"ok", "Approve", "approved!", "yes, go ahead", "looks good", "Go ahead with the plan", "утверждаю", "Да", "согласен", "поехали"} {
		if !ApprovesPlan(m) {
			t.Errorf("%q not read as approval", m)
		}
	}
	for _, m := range []string{"ok but split stage 3", "approve after you add a Version dimension", "why stage 2?", "no", "change the order", "да, но без стадии 4"} {
		if ApprovesPlan(m) {
			t.Errorf("%q read as approval", m)
		}
	}
	plan := &TaskPlan{Stages: []TaskStage{{Title: "Structure"}, {Title: "P&L"}}}
	if m := PlanApprovedMessage(plan); !strings.HasPrefix(m, ContinuePrefix+"Plan approved") || !strings.Contains(m, "stage 1 (Structure)") {
		t.Errorf("approved: %s", m)
	}
}

// A dashboard of the target names its grids; one saved as a name alone
// still reads (the scheme draws it without lines).
func TestTargetPagesReadBothForms(t *testing.T) {
	var tm TargetModel
	if err := json.Unmarshal([]byte(`{"summary":"x","pages":["Start here",{"name":"Board","audience":"CFO","grids":["P&L","Cash"]}]}`), &tm); err != nil {
		t.Fatal(err)
	}
	if len(tm.Pages) != 2 || tm.Pages[0].Name != "Start here" || len(tm.Pages[0].Grids) != 0 || tm.Pages[1].Audience != "CFO" || len(tm.Pages[1].Grids) != 2 {
		t.Fatalf("pages: %+v", tm.Pages)
	}
	if got := tm.Pages[1].String(); got != "Board (CFO) — shows P&L, Cash" {
		t.Errorf("page line: %q", got)
	}
}

// Every dashboard of a target names the grids it shows (a guide shows
// none); update_target_model links the dashboards of an earlier target.
func TestTargetPagesNameTheirGrids(t *testing.T) {
	base := func(pages ...TargetPage) *TargetModel {
		return &TargetModel{Summary: "x", Sheets: []TargetSheet{{Sheet: "OPEX", Role: "input", Grid: "OPEX", Metrics: []string{"opex"}}}, Pages: pages}
	}
	if _, err := CheckTarget(base(TargetPage{Name: "Board"}), nil); err == nil || !strings.Contains(err.Error(), `dashboard "Board" names no grids`) {
		t.Errorf("unlinked dashboard: %v", err)
	}
	if _, err := CheckTarget(base(TargetPage{Name: "Board", Grids: []string{"P&L"}}), nil); err == nil || !strings.Contains(err.Error(), `grid "P&L"`) {
		t.Errorf("unknown grid: %v", err)
	}
	if _, err := CheckTarget(base(TargetPage{Name: "Board", Grids: []string{"opex"}}, TargetPage{Name: "Start here", Guide: true}), nil); err != nil {
		t.Errorf("linked: %v", err)
	}
	old := base(TargetPage{Name: "Board"})
	if !strings.Contains(old.Render(), "Dashboards not linked to grids yet: Board") {
		t.Error("an unlinked dashboard is not pointed out to the AI")
	}
	if _, err := old.Apply(TargetUpdate{Pages: []TargetPage{{Name: "board", Grids: []string{"OPEX"}}}}); err != nil || len(old.UnlinkedPages()) != 0 {
		t.Errorf("linking by update: %v, unlinked %v", err, old.UnlinkedPages())
	}
}
