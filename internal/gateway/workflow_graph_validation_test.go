package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// Table-driven coverage for the workflow graph validator. The pre-existing
// validator checked names, types, assignee roles, required routes and that
// route targets resolve; it did not check that step IDs are unique, that
// every step is reachable, or that any path can finish — so a definition with
// a dead branch or an inescapable loop published cleanly and then stalled at
// runtime with nothing to point at.
//
// Cycles are deliberately NOT rejected: a reject route looping back for rework
// is how this engine expresses rework, and activateNextSteps handles revisits.
// An inescapable loop is caught as "no reachable end" instead.
func TestValidateWorkflowDefGraph(t *testing.T) {
	step := func(id, name, stepType string, routes map[string]string) map[string]any {
		s := map[string]any{"id": id, "name": name, "type": stepType}
		if routes != nil {
			r := map[string]any{}
			for k, v := range routes {
				r[k] = v
			}
			s["routes"] = r
		}
		return s
	}
	// A task step needs an assignee role to satisfy the pre-existing checks,
	// so graph errors are the only ones under test.
	task := func(id, name string, routes map[string]string) map[string]any {
		s := step(id, name, "task", routes)
		s["assignee_roles"] = []any{"business_user"}
		return s
	}

	for _, tc := range []struct {
		name      string
		steps     []map[string]any
		wantErr   string // substring; "" means the definition must validate
		wantNoErr string // substring that must NOT appear
	}{
		{
			name:  "linear workflow with sequential fallback is valid",
			steps: []map[string]any{task("a", "First", nil), task("b", "Second", nil)},
		},
		{
			name:  "explicit end route is valid",
			steps: []map[string]any{task("a", "First", map[string]string{"next": "end-done"})},
		},
		{
			name:    "duplicate step ids are rejected",
			steps:   []map[string]any{task("a", "First", nil), task("a", "Also First", nil)},
			wantErr: "Duplicate step id",
		},
		{
			// A rework loop with a person on it is legitimate: reject sends
			// the draft back, the engine re-activates it.
			name: "a route back to a task is a rework loop and is valid",
			steps: []map[string]any{
				task("draft", "Draft", map[string]string{"next": "review"}),
				step("review", "Review", "approval", map[string]string{"approve": "end-done", "reject": "draft"}),
			},
			wantNoErr: "loop",
		},
		{
			// Nobody could ever stop a loop made only of automatic steps.
			name: "a loop with no task or approval on it is rejected",
			steps: []map[string]any{
				task("start", "Start", map[string]string{"next": "check"}),
				step("check", "Check", "condition", map[string]string{"true": "end-done", "false": "notify"}),
				step("notify", "Notify", "notification", map[string]string{"next": "check"}),
			},
			wantErr: `Route from "Notify" back to "Check" creates a loop with no task or approval`,
		},
		{
			name: "a diamond that rejoins is not a loop",
			steps: []map[string]any{
				step("c", "Big?", "condition", map[string]string{"true": "a", "false": "b"}),
				task("a", "A", map[string]string{"next": "j"}),
				task("b", "B", map[string]string{"next": "j"}),
				step("j", "Join", "join", map[string]string{"next": "end-done"}),
			},
			wantNoErr: "loop",
		},
		{
			name:    "a step with no id is rejected",
			steps:   []map[string]any{step("", "Nameless", "task", nil)},
			wantErr: "has no id",
		},
		{
			name: "unreachable step is rejected",
			steps: []map[string]any{
				task("a", "First", map[string]string{"next": "end-done"}),
				task("orphan", "Orphan", map[string]string{"next": "end-done"}),
			},
			wantErr: "is unreachable",
		},
		{
			name: "inescapable loop is rejected",
			steps: []map[string]any{
				task("a", "First", map[string]string{"next": "b"}),
				task("b", "Second", map[string]string{"next": "a"}),
			},
			wantErr: "no reachable end",
		},
		{
			name: "rework loop with an exit is allowed",
			steps: []map[string]any{
				task("submit", "Submit", map[string]string{"next": "review"}),
				step("review", "Review", "approval", map[string]string{"approve": "end-done", "reject": "submit"}),
			},
			wantNoErr: "no reachable end",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.steps)
			if err != nil {
				t.Fatalf("marshal steps: %v", err)
			}
			// approval steps need an approver role to clear the pre-existing checks
			var withRoles []map[string]any
			_ = json.Unmarshal(raw, &withRoles)
			for _, s := range withRoles {
				if s["type"] == "approval" {
					s["approver_roles"] = []any{"business_admin"}
				}
			}
			raw, _ = json.Marshal(withRoles)

			errs := workflow.ValidateDef(&workflow.WorkflowDefFull{Name: "Flow", Steps: raw})
			joined := strings.Join(errs, "; ")
			if tc.wantErr != "" && !strings.Contains(joined, tc.wantErr) {
				t.Errorf("want an error containing %q, got: %v", tc.wantErr, errs)
			}
			if tc.wantErr == "" && tc.wantNoErr == "" && len(errs) > 0 {
				t.Errorf("want a valid definition, got errors: %v", errs)
			}
			if tc.wantNoErr != "" && strings.Contains(joined, tc.wantNoErr) {
				t.Errorf("must not report %q, got: %v", tc.wantNoErr, errs)
			}
		})
	}
}
