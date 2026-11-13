package aiassistant

import (
	"encoding/json"
	"strings"
	"testing"
)

// A stage is often several plans: its items count as done plan by plan, and
// it finishes when its confirmed plans did every item between them; a stage
// that loads values is finished by its numbers, not its steps (2026-11-12).
func TestStageCoverageAddsUpOverPlans(t *testing.T) {
	plan, err := NewTaskPlan("Build", []TaskStage{
		{Title: "Calculation sheets", Items: []string{"Headcount sheet metrics", "SaaS sheet metrics"}},
		{Title: "Inputs", Items: []string{"Assumptions driver values loaded from the workbook"}}})
	if err != nil {
		t.Fatal(err)
	}
	plan.NoteCoverage("p1", []string{"Headcount sheet metrics"})
	plan.RecordConfirmed("p1", 9, true)
	st := plan.Stages[0]
	if st.Status != StageInProgress || len(st.OpenItems()) != 1 || st.OpenItems()[0] != "SaaS sheet metrics" {
		t.Fatalf("after one of two items: %+v", st)
	}
	if r := plan.Render(); !strings.Contains(r, "still open: SaaS sheet metrics") {
		t.Errorf("render does not say what is open:\n%s", r)
	}
	plan.NoteCoverage("p2", []string{"SaaS sheet metrics"})
	plan.RecordConfirmed("p2", 4, false) // a step failed: nothing counts
	if plan.Stages[0].Status == StageDone {
		t.Fatal("a partly run plan finished the stage")
	}
	plan.NoteCoverage("p3", []string{"SaaS sheet metrics"})
	plan.RecordConfirmed("p3", 4, true)
	if plan.Stages[0].Status != StageDone {
		t.Fatalf("both items done, stage not finished: %+v", plan.Stages[0])
	}
	// The values stage: every item has a confirmed step, its numbers decide.
	if !plan.Stages[1].NeedsProof() || plan.Stages[0].NeedsProof() {
		t.Fatal("NeedsProof misreads the stages")
	}
	plan.MarkCovered("p4")
	plan.RecordConfirmed("p4", 9, true)
	if plan.Stages[1].Status == StageDone || !strings.Contains(plan.Stages[1].Note, "not proven") {
		t.Fatalf("values stage closed by its steps: %+v", plan.Stages[1])
	}
}

// The same stage sent again is not added twice; a re-cut keeps what is done;
// a done stage is never turned skipped; items may come as one string.
func TestTaskPlanAppendReplaceAndItems(t *testing.T) {
	plan, _ := NewTaskPlan("Build", []TaskStage{
		{Title: "Stage 1 — Structure", Items: []string{"dimensions"}},
		{Title: "Stage 2 — Calculation sheets and formulas", Items: []string{"all sheets"}}})
	plan.Stages[0].Status = StageDone
	if dup := plan.Append("Build", []TaskStage{{Title: "Structure", Items: []string{"x"}}, {Title: "Calculation sheets and formulas", Items: []string{"y"}}, {Title: "Dashboards", Items: []string{"pages"}}}); len(dup) != 2 || len(plan.Stages) != 3 {
		t.Fatalf("append: dup %v, stages %d", dup, len(plan.Stages))
	}
	plan.Replace("", []TaskStage{{Title: "Headcount", Items: []string{"Headcount rows"}}, {Title: "SaaS", Items: []string{"SaaS rows"}}})
	if len(plan.Stages) != 3 || plan.Stages[0].Status != StageDone || plan.Stages[1].Title != "Headcount" || plan.Current() != 1 {
		t.Fatalf("replace: %+v", plan.Stages)
	}
	if err := plan.UpdateStage(1, StageSkipped, "replaced"); err == nil || plan.Stages[0].Status != StageDone {
		t.Fatalf("done stage turned skipped (%v)", err)
	}
	var st TaskStage
	if err := json.Unmarshal([]byte(`{"title":"T","items":"Month dimension; Scenario dimension"}`), &st); err != nil || len(st.Items) != 2 || st.Items[1] != "Scenario dimension" {
		t.Fatalf("items as a string: %+v %v", st, err)
	}
	if err := json.Unmarshal([]byte(`{"title":"T","items":["a"],"status":"done","covered":["a"]}`), &st); err != nil || st.Status != StageDone || len(st.Covered) != 1 {
		t.Fatalf("stored stage round trip: %+v %v", st, err)
	}
	if err := CheckStages([]TaskStage{{Title: "One", Items: []string{"a"}}}); err != nil {
		t.Fatalf("one stage refused for an append: %v", err)
	}
}

// The carried-plan check finishes no stage: it read stages with nothing built
// as whole, twice (2026-11-12). It notes what it read.
func TestMarkFoundLeavesValueStagesOpen(t *testing.T) {
	plan, _ := NewTaskPlan("Build", []TaskStage{
		{Title: "Structure", Items: []string{"Month dimension"}},
		{Title: "Calculation sheets", Items: []string{"Headcount formulas compared with the workbook"}}})
	plan.MarkFound(map[int][]string{0: {"Month dimension"}, 1: {"Headcount formulas compared with the workbook"}})
	if plan.Stages[0].Status == StageDone || plan.Stages[1].Status == StageDone || !strings.Contains(plan.Stages[0].Note, "unverified") {
		t.Fatalf("found: %+v", plan.Stages)
	}
}

// A plan holding only carried decisions (or a decision recorded before
// plan_task) has no stages; the console reads stages.length, so stages and a
// stage's items go out as arrays, and a stored null heals on the way out
// (a carried decisions-only plan stopped the AI developer tab, 2026-11-13).
func TestTaskPlanStagesNeverNull(t *testing.T) {
	b, err := json.Marshal(&TaskPlan{Decisions: []string{"store percentages in percent units"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"stages":[]`) {
		t.Fatalf("decisions-only plan: want stages [], got %s", b)
	}
	var stored TaskPlan
	if err := json.Unmarshal([]byte(`{"goal":"g","stages":[{"title":"t","items":null,"status":"pending"}],"decisions":["d"]}`), &stored); err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(map[string]any{"task_plan": &stored})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"items":[]`) || strings.Contains(string(b), "null") {
		t.Fatalf("stored plan: want items [] and no null, got %s", b)
	}
	stored.Stages = nil
	if b, _ = json.Marshal(stored); !strings.Contains(string(b), `"stages":[]`) {
		t.Fatalf("stored null stages: want [], got %s", b)
	}
}
