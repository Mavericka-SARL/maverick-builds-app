package aiassistant

import (
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"strings"
	"testing"
	"time"
)

// A message is read as a direct order, a delegated goal or a question, and
// the turn's instructions follow (owner, 2026-11-08).
func TestClassifyByRules(t *testing.T) {
	plan, _ := NewTaskPlan("Build the model", []TaskStage{{Title: "Dimensions", Items: []string{"Month"}}, {Title: "Inputs", Items: []string{"price"}}})
	for msg, want := range map[string]string{
		"Create dimension Region with members North, South and West":  ModeDirect,
		"Rename metric rev to revenue and set its format to currency": ModeDirect,
		"Build a model based on the attached Excel":                   ModeDelegated,
		"Make the P&L match the workbook":                             ModeDelegated,
		"Fix whatever differs on Cash_Flow":                           ModeDelegated,
		"Which metrics feed EBITDA?":                                  ModeQuestion,
	} {
		if got := ClassifyByRules(msg, nil).Mode; got != want {
			t.Errorf("%q read as %s, want %s", msg, got, want)
		}
	}
	if got := ClassifyByRules("Confirmed. Continue.", plan).Mode; got != ModeDelegated {
		t.Errorf("a continuation with a plan in progress read as %s", got)
	}
	if got := ClassifyByRules("Confirmed. Continue.", nil).Mode; got != ModeDirect {
		t.Errorf("a continuation without a plan read as %s", got)
	}
}

func TestParseClassification(t *testing.T) {
	m, ok := ParseClassification("Sure: {\"mode\": \"delegated\", \"tasks\": [\"pages\", \"nonsense\", \"pages\", \"records\"]}")
	if !ok || m.Mode != ModeDelegated || strings.Join(m.Tasks, ",") != "pages,records" {
		t.Errorf("ParseClassification = %+v %v", m, ok)
	}
	if _, ok := ParseClassification(`{"mode": "maybe"}`); ok {
		t.Error("an unknown mode was accepted")
	}
}

func TestTurnBriefCarriesModePlaybooksAndPlan(t *testing.T) {
	plan, err := NewTaskPlan("Add the pages", []TaskStage{
		{Title: "Demand & revenue", Items: []string{"page Demand & revenue"}, Check: "describe_dashboard"},
		{Title: "Variance", Items: []string{"page Variance"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	direct := TurnBrief(RequestMode{Mode: ModeDirect}, nil)
	if !strings.Contains(direct, "DIRECT ORDER") || strings.Contains(direct, "Playbook") {
		t.Errorf("direct brief: %s", direct)
	}
	if err := plan.UpdateStage(1, StageDone, "3 of 3 widgets"); err != nil {
		t.Fatal(err)
	}
	delegated := TurnBrief(RequestMode{Mode: ModeDelegated, Tasks: []string{"pages"}}, plan)
	for _, want := range []string{"DELEGATED A GOAL", "### Playbook: pages", "1. [done] Demand & revenue", "2. [to do, CURRENT] Variance — items: page Variance", "Work on the CURRENT stage"} {
		if !strings.Contains(delegated, want) {
			t.Errorf("delegated brief lacks %q:\n%s", want, delegated)
		}
	}
}

func TestTaskPlanRules(t *testing.T) {
	if _, err := NewTaskPlan("x", []TaskStage{{Title: "one", Items: []string{"a"}}}); err == nil {
		t.Error("a one-stage plan was accepted")
	}
	if _, err := NewTaskPlan("x", []TaskStage{{Title: "one", Items: []string{"a"}}, {Title: "two"}}); err == nil || !strings.Contains(err.Error(), "stage 2 needs") {
		t.Errorf("a stage without items: %v", err)
	}
	p, _ := NewTaskPlan("x", []TaskStage{{Title: "one", Items: []string{"a"}}, {Title: "two", Items: []string{"b"}}})
	if err := p.UpdateStage(2, StageSkipped, ""); err == nil {
		t.Error("a skip without a note was accepted")
	}
	_ = p.UpdateStage(1, StageDone, "")
	_ = p.UpdateStage(2, StageSkipped, "the developer dropped it")
	if p.Current() != -1 {
		t.Errorf("Current() = %d with every stage done or skipped", p.Current())
	}
	if !strings.Contains(TurnBrief(RequestMode{Mode: ModeDelegated}, p), "Every stage is done or skipped") {
		t.Error("the finished plan's brief does not ask for the report")
	}
}

// A stage the coverage check found whole is finished by its confirmed plan;
// decisions are kept once each and shown; a new goal's stages follow the
// plan under way; found items finish a stage or are noted.
func TestTaskPlanCarriedContext(t *testing.T) {
	plan, err := NewTaskPlan("Build the model", []TaskStage{
		{Title: "Inputs", Items: []string{"drivers"}}, {Title: "Engine", Items: []string{"revenue", "costs"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.MarkCovered("p1") || !plan.RecordConfirmed("p1", 12, true) || plan.Stages[0].Status != StageDone {
		t.Fatalf("covered stage not finished: %+v", plan.Stages[0])
	}
	plan.MarkCovered("p2")
	plan.RecordConfirmed("p2", 5, false) // a step failed: the stage stays open
	if plan.Stages[1].Status != StageInProgress {
		t.Fatalf("partly run plan finished its stage: %+v", plan.Stages[1])
	}
	if !plan.AddDecision("Asked: percent or fraction? — the developer answered: percent units") || plan.AddDecision("asked: percent or fraction? — the developer answered: percent units") {
		t.Fatal("a decision was not kept once")
	}
	if r := plan.Render(); !strings.Contains(r, "never ask them again:\n- Asked: percent or fraction?") {
		t.Fatalf("render lacks the decision:\n%s", r)
	}
	plan.MarkFound(map[int][]string{1: {"revenue"}})
	if !strings.HasSuffix(plan.Stages[1].Note, "(unverified — check, then mark the stage done): revenue") || plan.Stages[1].Status == StageDone {
		t.Fatalf("partly found stage: %+v", plan.Stages[1])
	}
	plan.Append("Add the pages", []TaskStage{{Title: "Pages", Items: []string{"P&L page"}, Status: StageDone}})
	if len(plan.Stages) != 3 || plan.Stages[2].Status != StagePending || !strings.Contains(plan.Goal, "then: Add the pages") {
		t.Fatalf("appended plan: %+v", plan)
	}
	only := &TaskPlan{Decisions: []string{"opening = last closing"}}
	if r := TurnBrief(RequestMode{Mode: ModeDirect}, only); !strings.Contains(r, "opening = last closing") || strings.Contains(r, "Every stage is done") {
		t.Fatalf("decisions-only brief:\n%s", r)
	}
}

// Tool exchanges before the last confirmed plan leave the history the model
// is sent; the last two turns stay whole, and so does every text.
func TestSettleHistory(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 11, 11, 10, m, 0, 0, time.UTC) }
	msgs := []ChatMessage{
		{Role: "user", Content: "build inputs", CreatedAt: at(1)},
		{Role: "assistant", Content: "", ToolCalls: []providers.ToolCall{{ID: "a", Name: "list_metrics"}}, CreatedAt: at(2)},
		{Role: "tool", Content: "metrics…", ToolCallID: "a", CreatedAt: at(3)},
		{Role: "assistant", Content: "Here is the plan.", ToolCalls: []providers.ToolCall{{ID: "b", Name: "propose_actions"}}, CreatedAt: at(4)},
		{Role: "tool", Content: "Proposal created", ToolCallID: "b", CreatedAt: at(5)},
		{Role: "user", Content: "next", CreatedAt: at(10)},
		{Role: "assistant", Content: "", ToolCalls: []providers.ToolCall{{ID: "c", Name: "list_grids"}}, CreatedAt: at(11)},
		{Role: "tool", Content: "grids…", ToolCallID: "c", CreatedAt: at(12)},
		{Role: "user", Content: "and now", CreatedAt: at(20)},
	}
	out := SettleHistory(msgs, at(8))
	if len(out) != 6 {
		t.Fatalf("%d messages kept, want 6: %+v", len(out), out)
	}
	if out[1].Content != "Here is the plan." || len(out[1].ToolCalls) != 0 {
		t.Fatalf("the plan's text was not kept without its call: %+v", out[1])
	}
	if out[3].ToolCalls == nil || out[4].Role != "tool" {
		t.Fatalf("the last two turns were not kept whole: %+v", out[2:])
	}
	if got := SettleHistory(msgs, time.Time{}); len(got) != len(msgs) {
		t.Fatal("nothing confirmed, yet history was dropped")
	}
}

func TestTurnToolsLeaveConnectorsOutOfABuild(t *testing.T) {
	has := func(tools []providers.ToolDef, name string) bool {
		for _, t := range tools {
			if t.Name == name {
				return true
			}
		}
		return false
	}
	build := TurnTools(RequestMode{Mode: ModeDelegated, Tasks: []string{"workbook_build"}}, "build the engine sheets")
	if has(build, "test_integration") || !has(build, "compare_with_sheet") || !has(build, "propose_actions") {
		t.Fatal("a build turn's tools are wrong")
	}
	for _, m := range []struct {
		mode RequestMode
		msg  string
	}{
		{RequestMode{Mode: ModeQuestion, Tasks: []string{"pages"}}, "what does the P&L page show?"},
		{RequestMode{Mode: ModeDelegated, Tasks: []string{"workbook_build"}}, "load the actuals from the REST API"},
		{RequestMode{Mode: ModeDelegated, Tasks: []string{"records"}}, "load the GL file"},
		{RequestMode{Mode: ModeDirect}, "rename revenue"},
	} {
		if !has(TurnTools(m.mode, m.msg), "test_integration") {
			t.Errorf("%q dropped the connector tools", m.msg)
		}
	}
}
