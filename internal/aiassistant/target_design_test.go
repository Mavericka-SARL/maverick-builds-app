package aiassistant

import (
	"strings"
	"testing"
)

func designTarget() *TargetModel {
	return &TargetModel{
		Summary: "Startup FP&A.",
		Conventions: TargetConventions{Units: "EUR", Signs: "costs positive", Percentages: "percent units; the workbook stores fractions",
			OpeningBalances: "the last closing", Scenarios: "Scenario members; the saved workbook shows Base", Aggregation: "flows sum, balances close"},
		Sheets: []TargetSheet{
			{Sheet: "Hiring Plan", Role: "input", Grid: "Hiring", Owner: "People"},
			{Sheet: "Headcount", Role: "calculation", Grid: "Headcount", Metrics: []string{"fte"}, Verification: "compare_with_sheet all rows"},
			{Sheet: "Import BS", Role: "actuals", Grid: "Balances"},
		},
		Decisions: []TargetDecision{{Question: "Do imported closing balances start the next forecast?",
			Options: []string{"reproduce the workbook (unused)", "start the forecast from them"}, Sheets: []string{"Import BS"}}},
		Requirements: []TargetRequirement{{Area: "Approved plan", Behaviour: "a frozen approved revision apart from the working forecast"}},
		Checks:       []TargetCheck{{Name: "Funding disabled", How: "turn the funding switch off and compare cash"}},
	}
}

// One agreed design (owner, 2026-11-12): a target of an attached workbook
// states its conventions; open decisions hold their sheets back until the
// developer chooses; a row changed on purpose says why.
func TestTargetDesignAgreedOnce(t *testing.T) {
	sheets := []string{"Hiring Plan", "Headcount", "Import BS"}
	tm := designTarget()
	tm.Conventions.Aggregation = ""
	if _, err := CheckTarget(tm, sheets); err == nil || !strings.Contains(err.Error(), "aggregation") {
		t.Fatalf("missing convention taken: %v", err)
	}
	tm = designTarget()
	tm.Sheets[1].Metrics = nil
	if _, err := CheckTarget(tm, sheets); err == nil || !strings.Contains(err.Error(), "needs its metric names") {
		t.Fatalf("a calculation sheet without metric names taken: %v", err)
	}
	if _, err := CheckTarget(designTarget(), nil); err != nil {
		t.Fatalf("a target with no workbook needs no conventions check: %v", err)
	}
	tm = designTarget()
	tm.Sheets[1].RowNotes = []TargetRowNote{{Row: 9, Status: "changed"}}
	if _, err := CheckTarget(tm, sheets); err == nil || !strings.Contains(err.Error(), "say why") {
		t.Fatalf("a changed row without a reason taken: %v", err)
	}
	tm = designTarget()
	if _, err := CheckTarget(tm, sheets); err != nil {
		t.Fatal(err)
	}
	if held := tm.HeldBack("import bs"); !strings.Contains(held, "closing balances") {
		t.Fatalf("Import BS not held back: %q", held)
	}
	if tm.HeldBack("Headcount") != "" {
		t.Fatal("a sheet no decision names was held back")
	}
	out := tm.Render()
	for _, want := range []string{"Conventions: units EUR", "OPEN DECISIONS", "holds back Import BS", "Approved plan", "Funding disabled [open]", "owner People"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}

	// The developer chooses; the check and requirement move on; a row gets
	// its meaning and another is marked changed on purpose.
	var u TargetUpdate
	u.Decisions = []TargetDecision{{Question: "do imported closing balances start the next forecast?", Choice: "start the forecast from them"}}
	u.Checks = []TargetCheck{{Name: "funding disabled", Status: "passed", Evidence: "cash 36/36 months equal the workbook with funding off"}}
	u.Requirements = []TargetRequirement{{Area: "approved plan", Status: "built"}}
	u.Rows = append(u.Rows, struct {
		Sheet string `json:"sheet"`
		TargetRowNote
	}{Sheet: "Headcount", TargetRowNote: TargetRowNote{Row: 12, Meaning: "FTE by function", Status: "changed", Reason: "one row per function becomes a Function dimension"}})
	resolved, err := tm.Apply(u)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || tm.HeldBack("Import BS") != "" {
		t.Fatalf("decision not resolved: %+v / %q", resolved, tm.HeldBack("Import BS"))
	}
	if len(tm.OpenChecks()) != 0 || tm.Requirements[0].Status != "built" {
		t.Fatalf("check or requirement not moved on: %+v %+v", tm.Checks, tm.Requirements)
	}
	if out := tm.RenderFor("Headcount"); !strings.Contains(out, "row 12: FTE by function [changed on purpose: one row per function") {
		t.Fatalf("row note not rendered:\n%s", out)
	}
	bad := TargetUpdate{Checks: []TargetCheck{{Name: "Funding disabled", Status: "maybe"}}}
	if _, err := tm.Apply(bad); err == nil {
		t.Fatal("an unknown check status taken")
	}
}

// A stage is a module of sheets, ends with its evidence and carries what it
// took (owner, 2026-11-12).
func TestStageModuleEvidenceMeasure(t *testing.T) {
	p, err := NewTaskPlan("Build", []TaskStage{
		{Title: "Hiring Plan → Headcount", Items: []string{"hiring loaded", "FTE"}, Sheets: []string{"Hiring Plan", "Headcount"}},
		{Title: "SaaS", Items: []string{"revenue"}, Sheet: "SaaS"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.Stages[0].StageSheets(), ","); got != "Hiring Plan,Headcount" {
		t.Fatalf("module sheets = %s", got)
	}
	if got := strings.Join(p.Stages[1].StageSheets(), ","); got != "SaaS" {
		t.Fatalf("single sheet = %s", got)
	}
	p.AddTurn(0, TurnMeasure{Minutes: 2, Calls: 7, ToolCalls: 5, InputTokens: 1000, CachedTokens: 50000, OutputTokens: 800})
	p.AddTurn(0, TurnMeasure{Minutes: 1.5, Calls: 3, ToolCalls: 2, Correction: true})
	p.AddTurn(9, TurnMeasure{Calls: 99}) // out of range: ignored
	m := p.Stages[0].Measure
	if m == nil || m.Turns != 2 || m.Calls != 10 || m.ToolCalls != 7 || m.Corrections != 1 || m.Minutes != 3.5 {
		t.Fatalf("measure = %+v", m)
	}
	p.Stages[0].Evidence = &StageEvidence{Built: "Hiring and Headcount grids", Compared: "Headcount 18/18 rows, 648/648 periods matching", Differences: "none", Next: "SaaS"}
	p.Stages[0].Status = StageDone
	out := p.Render()
	for _, want := range []string{"built: Hiring and Headcount grids", "compared: Headcount 18/18 rows", "took 4 min, 2 turns, 10 calls", "1 corrections"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}
