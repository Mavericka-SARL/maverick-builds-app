package aiassistant

import (
	"strings"
	"testing"
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
