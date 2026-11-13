package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

func TestSplitCoverageKeepsStageItemsApart(t *testing.T) {
	stage := []string{"Formula metrics and grids for Headcount, SaaS and Hardware", "Values loaded from the workbook for all driver inputs"}
	req, st := splitCoverage([]string{"formula metrics and grids for headcount, saas and hardware", "Funding Plan grid"}, stage)
	if len(req) != 1 || req[0] != "Funding Plan grid" || len(st) != 1 || st[0] != stage[0] {
		t.Fatalf("request %v, stage %v", req, st)
	}
}

// Replies of 2026-11-12 that ended a turn naming the next work, proposing
// nothing — and ones that must pass (a question, a stop, a done report).
func TestAnnouncesOwnWork(t *testing.T) {
	for _, r := range []string{
		"I've checked the current model state.\n\nI'll continue with a corrected proposal for that next.",
		"I'm correcting that by reshaping the Funding Plan input grid to single-value rows and then writing the workbook's values.",
		"The last proposal failed because it mixed a non-existent metric ID. I need to resubmit this as a clean create-and-grid bundle.",
		"Next step remains the same: load the rest of the Assumptions drivers from the workbook into the existing grid shape.",
		"I'll continue from there.",
	} {
		if !announcesOwnWork(r) {
			t.Errorf("not caught: %q", r)
		}
	}
	for _, r := range []string{
		"Should I use the workbook's rows exactly, or refactor them into a multidimensional structure?",
		"Stage 4 is done: all 31 rows of the Annual Summary match in the three scenarios.",
		"I need to stop here.",
	} {
		if announcesOwnWork(r) {
			t.Errorf("caught: %q", r)
		}
	}
}

func TestSheetNamedIn(t *testing.T) {
	text := " " + strings.ToLower("Assumptions values loaded; compare the P&L and Cash Flow sheets; saasy") + " "
	for s, want := range map[string]bool{"assumptions": true, "p&l": true, "cash flow": true, "saas": false, "control": false} {
		if got := sheetNamedIn(text, s); got != want {
			t.Errorf("%s: %v", s, got)
		}
	}
}

// plan_task adds one stage to a plan under way, re-cuts it with replace,
// and does not add a stage twice; a values stage marked done is checked by
// its numbers once, then taken with a note.
func TestPlanTaskAppendReplaceAndProof(t *testing.T) {
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
	call := func(name string, args any, proof func(aiassistant.TaskStage) string) (string, error) {
		raw, _ := json.Marshal(args)
		return runTool(ctx, chat, s.ID, taskHooks{Proof: proof}, nil, name, raw)
	}
	stage := func(title, item string) map[string]any {
		return map[string]any{"title": title, "items": []string{item}}
	}
	if _, err := call(aiassistant.PlanTaskTool, map[string]any{"goal": "Build", "stages": []any{stage("Structure", "dimensions"), stage("Inputs", "Assumptions values loaded")}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := call(aiassistant.PlanTaskTool, map[string]any{"goal": "Build", "stages": []any{stage("Stage 2 — Inputs", "x"), stage("Headcount", "Headcount rows")}}, nil); err != nil {
		t.Fatal(err)
	}
	plan, _ := chat.TaskPlan(ctx, s.ID)
	if len(plan.Stages) != 3 {
		t.Fatalf("append: %d stages, want 3 (Inputs not twice)", len(plan.Stages))
	}
	if _, err := call(aiassistant.PlanTaskTool, map[string]any{"goal": "Build", "replace": true, "stages": []any{stage("SaaS", "SaaS rows")}}, nil); err != nil {
		t.Fatal(err)
	}
	plan, _ = chat.TaskPlan(ctx, s.ID)
	if len(plan.Stages) != 1 || plan.Stages[0].Title != "SaaS" {
		t.Fatalf("replace: %+v", plan.Stages)
	}
	plan.Stages = append(plan.Stages, aiassistant.TaskStage{Title: "Assumptions", Items: []string{"Assumptions values loaded"}, Status: aiassistant.StagePending})
	_ = chat.SetTaskPlan(ctx, s.ID, plan)
	notProven := func(aiassistant.TaskStage) string { return "Assumptions: no row compared yet" }
	if _, err := call(aiassistant.UpdateTaskPlanTool, map[string]any{"stage": 2, "status": "done"}, nil); err == nil || !strings.Contains(err.Error(), "evidence") {
		t.Fatalf("done without evidence taken: %v", err)
	}
	if _, err := call(aiassistant.UpdateTaskPlanTool, map[string]any{"stage": 2, "status": "done", "evidence": map[string]any{"built": "Assumptions loaded", "differences": "none", "next": "SaaS"}}, notProven); err == nil || !strings.Contains(err.Error(), "not done by its numbers") {
		t.Fatalf("unproven done taken: %v", err)
	}
	if _, err := call(aiassistant.UpdateTaskPlanTool, map[string]any{"stage": 2, "status": "done", "note": "the sheet is scenario columns, compared by hand", "evidence": map[string]any{"built": "Assumptions loaded", "differences": "none", "next": "SaaS"}}, notProven); err != nil {
		t.Fatal(err)
	}
	plan, _ = chat.TaskPlan(ctx, s.ID)
	if st := plan.Stages[1]; st.Status != aiassistant.StageDone || !strings.HasPrefix(st.Note, "not proven by the workbook map") || st.Evidence == nil || st.Evidence.Next != "SaaS" {
		t.Fatalf("second done: %+v", st)
	}
}

// An import of a file the assistant typed is refused while the developer's
// workbook is attached, unless the request names the file or asks for
// sample data.
func TestTypedFileImportRefused(t *testing.T) {
	steps := []aiassistant.ProposalStep{
		{Tool: "import_file_data", Params: json.RawMessage(`{"file":"Assumptions_converted_from_workbook.csv","target_id":"Assumptions"}`)},
		{Tool: "import_file_data", Params: json.RawMessage(`{"file":"High_Tech_Startup_Monthly_FPA_Model (converted).csv"}`)},
	}
	typed := map[string]bool{"assumptions_converted_from_workbook.csv": true}
	probs, failed := typedFileProblems(steps, "Continue.", typed)
	if len(probs) != 1 || len(failed) != 1 || failed[0] != 0 || !strings.Contains(probs[0], "you wrote this file yourself") {
		t.Fatalf("typed import: %v %v", probs, failed)
	}
	if p, _ := typedFileProblems(steps, "Load sample values into the grid", typed); len(p) != 0 {
		t.Errorf("sample data asked for, still refused: %v", p)
	}
	if p, _ := typedFileProblems(steps, "import Assumptions_converted_from_workbook.csv", typed); len(p) != 0 {
		t.Errorf("file named by the developer, still refused: %v", p)
	}
}

// Typographic apostrophes do not hide a reply from the checks.
func TestReplyChecksReadTypographicApostrophes(t *testing.T) {
	if !announcesOwnWork("Understood — I’ll proceed exactly as you ordered.") {
		t.Error("announcesOwnWork missed I’ll")
	}
	if !claimsUnproposedChange("I’ll send the corrected proposal now.") {
		t.Error("claimsUnproposedChange missed I’ll")
	}
	if !pointsAtPlan.MatchString(plainApostrophes("I’ve prepared the plan — please confirm above.")) {
		t.Error("pointsAtPlan missed I’ve")
	}
}

// Plurals announce a plan too.
func TestAnnouncesProposalPlural(t *testing.T) {
	for _, r := range []string{
		"Understood — I’ll prepare and present the write proposals now (create the missing 14 Assumptions input metrics).",
		"Working on it — I’ll prepare the corrected propose_actions now.",
	} {
		if !claimsUnproposedChange(r) {
			t.Errorf("not caught: %q", r)
		}
	}
}

// A question about splitting the work is the server's to answer; a real
// choice about the model is not.
func TestAsksHowToSplit(t *testing.T) {
	for _, r := range []string{
		"Do you want me to\n- A) run this as two consecutive proposals now, or\n- B) run it as three or more smaller proposals?\nChoose A or B.",
		"Shall I split this into batches of 50 metrics?",
	} {
		if !asksHowToSplit(r) {
			t.Errorf("not caught: %q", r)
		}
	}
	for _, r := range []string{
		"Should opening cash come from Control or from the Funding Plan?",
		"Stage 4 is done: the P&L matches in all three scenarios.",
	} {
		if asksHowToSplit(r) {
			t.Errorf("caught: %q", r)
		}
	}
}

// A stage is one coherent module (owner, 2026-11-12): a few dependent
// sheets stay together and are proven together; more are cut into modules of
// at most maxModuleSheets in the workbook's order; a few pages stay one
// stage and more are cut into stages of maxStagePages.
func TestCutStages(t *testing.T) {
	sheets := []string{"Guide", "Hiring Plan", "Headcount", "SaaS", "Hardware", "P&L", "Cash Flow", "Balance Sheet"}
	in := []aiassistant.TaskStage{
		{Title: "Hiring Plan → Headcount → payroll", Items: []string{"hiring events loaded", "FTE and payroll"}},
		{Title: "Inputs", Items: []string{"Hiring Plan drivers by scenario"}},
		{Title: "Calculation sheets", Items: []string{"Formula metrics for SaaS, Hardware, P&L, Cash Flow and Balance Sheet", "Workbook comparisons for every calculated row"}},
		{Title: "Dashboards", Items: []string{"Start page", "Board dashboard"}},
		{Title: "Pages", Items: []string{"Start", "Board", "Statements", "SaaS drivers", "Actuals"}},
		{Title: "Roles and access", Items: []string{"Hardware Operations edits Hardware drivers"}},
	}
	out, cut := cutStages(in, sheets)
	var titles []string
	for _, s := range out {
		titles = append(titles, s.Title+"="+strings.Join(s.StageSheets(), "+"))
	}
	want := "Hiring Plan → Headcount → payroll=Hiring Plan+Headcount|Inputs=Hiring Plan|SaaS → Hardware → P&L → Cash Flow=SaaS+Hardware+P&L+Cash Flow|" +
		"Balance Sheet=Balance Sheet|Dashboards=|Dashboards: Start, Board, Statements=|Dashboards: SaaS drivers, Actuals=|Roles and access="
	if got := strings.Join(titles, "|"); got != want || len(cut) != 2 {
		t.Fatalf("stages\n%s\nwant\n%s\n(cut %v)", got, want, cut)
	}
	if last := out[3]; !strings.Contains(strings.Join(last.Items, ";"), "Workbook comparisons") {
		t.Errorf("generic item not kept with the last module: %+v", last)
	}
}

// A workbook build: plan_task is refused until a target is recorded, and the
// plan it makes keeps the target.
func TestTargetModelBeforePlan(t *testing.T) {
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
	hooks := taskHooks{Sheets: func() []string { return []string{"Guide", "Assumptions", "Headcount"} }}
	call := func(name string, args any) (string, error) {
		raw, _ := json.Marshal(args)
		return runTool(ctx, chat, s.ID, hooks, nil, name, raw)
	}
	stages := []any{map[string]any{"title": "Assumptions", "items": []string{"drivers loaded"}}, map[string]any{"title": "Headcount", "items": []string{"FTE rows"}}}
	if _, err := call(aiassistant.PlanTaskTool, map[string]any{"goal": "Build", "stages": stages}); err == nil || !strings.Contains(err.Error(), "set_target_model") {
		t.Fatalf("plan without a target taken: %v", err)
	}
	conventions := map[string]any{"units": "EUR", "signs": "costs positive", "percentages": "percent units; the workbook stores fractions",
		"opening_balances": "n/a", "scenarios": "Base only", "aggregation": "monthly sums"}
	if _, err := call(aiassistant.SetTargetModelTool, map[string]any{"summary": "A driver model.", "sheets": []any{
		map[string]any{"sheet": "Assumptions", "role": "input", "grid": "Assumptions"}}}); err == nil || !strings.Contains(err.Error(), "conventions") {
		t.Fatalf("a target without conventions taken: %v", err)
	}
	target := map[string]any{"summary": "A driver model.", "conventions": conventions, "sheets": []any{
		map[string]any{"sheet": "Assumptions", "role": "input", "grid": "Assumptions", "dimensions": []string{"scenario"}, "metrics": []string{"arpu"}},
		map[string]any{"sheet": "headcount", "role": "calculation", "grid": "Headcount", "metrics": []string{"fte"}},
	}}
	out, err := call(aiassistant.SetTargetModelTool, target)
	if err != nil || !strings.Contains(out, "Guide") {
		t.Fatalf("target: %v\n%s", err, out)
	}
	if _, err := call(aiassistant.PlanTaskTool, map[string]any{"goal": "Build", "stages": stages}); err != nil {
		t.Fatal(err)
	}
	plan, _ := chat.TaskPlan(ctx, s.ID)
	if plan.Target == nil || plan.Target.Sheets[1].Sheet != "Headcount" || plan.Stages[0].Sheet != "Assumptions" {
		t.Fatalf("plan after target: %+v", plan)
	}
	if !strings.Contains(plan.Render(), "TARGET MODEL") {
		t.Errorf("target not rendered")
	}
}

// Build order: calculation modules, then actuals, then the business
// requirements, then the verification checks (owner, 2026-11-12).
func TestBuildOrder(t *testing.T) {
	target := &aiassistant.TargetModel{Sheets: []aiassistant.TargetSheet{{Sheet: "Import GL", Role: "actuals"}, {Sheet: "P&L", Role: "calculation"}}}
	in := []aiassistant.TaskStage{
		{Title: "Import GL — postings", Sheet: "Import GL"}, {Title: "Dashboards: board, statements"}, {Title: "P&L", Sheet: "P&L"},
		{Title: "Final verification", Sheets: []string{"Checks"}}, {Title: "Roles and access"}, {Title: "Actuals P&L import loader"},
	}
	out := buildOrder(in, target)
	var titles []string
	for _, s := range out {
		titles = append(titles, s.Title)
	}
	if got := strings.Join(titles, " | "); got != "P&L | Import GL — postings | Actuals P&L import loader | Dashboards: board, statements | Roles and access | Final verification" {
		t.Fatalf("order: %s", got)
	}
	if buildOrder(out, target) != nil {
		t.Error("an ordered plan was reordered")
	}
}
