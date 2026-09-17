package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ValidateDef is the one definition of "this workflow could run": the
// developer console's Validate button and Publish gate call it, and so does
// the AI Developer before it proposes steps. It lived in internal/gateway
// until the assistant needed it too (2026-09-16) — the same move
// internal/metricformula made for formulas, and for the same reason: two
// hand-rolled copies of a rule drift, and the AI ends up either stricter or
// looser than the developer it is meant to match.
// validateWorkflowDef runs structural validation and returns a list of error strings.
func ValidateDef(def *WorkflowDefFull) []string {
	var errs []string
	if def.Name == "" {
		errs = append(errs, "Missing workflow name")
	}

	var steps []map[string]any
	if err := json.Unmarshal(def.Steps, &steps); err != nil || len(steps) == 0 {
		errs = append(errs, "Workflow has no steps")
		return errs
	}

	// Step IDs must exist and be unique: routing resolves a target by
	// scanning the step list for a matching ID (activateNextSteps), so a
	// duplicate makes every route to it non-deterministic and a missing one
	// makes the step unroutable.
	stepIDs := map[string]bool{}
	for i, s := range steps {
		id, _ := s["id"].(string)
		if id == "" {
			errs = append(errs, fmt.Sprintf("Step %d has no id", i+1))
			continue
		}
		if stepIDs[id] {
			errs = append(errs, fmt.Sprintf("Duplicate step id %q — routes to it are ambiguous", id))
			continue
		}
		stepIDs[id] = true
	}

	for _, s := range steps {
		name, _ := s["name"].(string)
		stepType, _ := s["type"].(string)
		label := name
		if label == "" {
			label = "unnamed step"
		}

		if name == "" {
			errs = append(errs, "Step has missing name")
		}
		if stepType == "" {
			errs = append(errs, fmt.Sprintf("Step %q has missing type", label))
		}

		routes, _ := s["routes"].(map[string]any)

		switch stepType {
		case "task":
			roles, _ := s["assignee_roles"].([]any)
			if len(roles) == 0 {
				errs = append(errs, fmt.Sprintf("Task step %q has no assignee role", label))
			}
		case "approval":
			roles, _ := s["assignee_roles"].([]any)
			if len(roles) == 0 {
				errs = append(errs, fmt.Sprintf("Approval step %q has no approver role", label))
			}
			if routes["approve"] == nil {
				errs = append(errs, fmt.Sprintf("Approval step %q missing approve route", label))
			}
			if routes["reject"] == nil {
				errs = append(errs, fmt.Sprintf("Approval step %q missing reject route", label))
			}
		case "notification":
			// "Specific role" with no role notifies nobody at runtime; the
			// editor even says so on the step — validation must too.
			if n, _ := s["notification"].(map[string]any); n != nil {
				if rt, _ := n["recipient_type"].(string); rt == "role" {
					if role, _ := n["recipient_role"].(string); strings.TrimSpace(role) == "" {
						errs = append(errs, fmt.Sprintf("Notification step %q is addressed to a role but no recipient role is set", label))
					}
				}
			}
		case "condition":
			if s["condition"] == nil {
				errs = append(errs, fmt.Sprintf("Condition step %q missing condition", label))
			}
			if routes["true"] == nil {
				errs = append(errs, fmt.Sprintf("Condition step %q missing true route", label))
			}
			if routes["false"] == nil {
				errs = append(errs, fmt.Sprintf("Condition step %q missing false route", label))
			}
		}

		for routeKey, routeVal := range routes {
			target, _ := routeVal.(string)
			if target != "" && !strings.HasPrefix(target, "end-") && !stepIDs[target] {
				errs = append(errs, fmt.Sprintf("Step %q route %q points to unknown step %q", label, routeKey, target))
			}
		}
	}

	errs = append(errs, ValidateGraph(steps)...)
	return errs
}

// ValidateGraph checks the reachability properties a definition needs
// to actually run to completion: every step must be reachable from the entry
// step, and at least one path must be able to finish.
//
// It models this engine's real routing rather than a textbook flowchart:
//
//   - A step with no routes falls through to the NEXT step in document order
//     (activateNextSteps' sequential fallback), so document order is part of
//     the graph, not just decoration.
//   - A target of "end-*" terminates the instance.
//   - Cycles are NOT rejected. A reject route looping back to an earlier step
//     is the normal way to express rework in this engine, and the runtime
//     handles revisits (see the RowsAffected guard in activateNextSteps).
//     A cycle with no way out is still a real defect, and is caught by the
//     termination check below rather than by banning loops outright.
func ValidateGraph(steps []map[string]any) []string {
	var errs []string
	if len(steps) == 0 {
		return errs
	}

	indexByID := make(map[string]int, len(steps))
	for i, s := range steps {
		if id, _ := s["id"].(string); id != "" {
			if _, dup := indexByID[id]; !dup {
				indexByID[id] = i
			}
		}
	}

	// successors returns the step indices a step can activate, plus whether
	// it can terminate the instance from here.
	successors := func(i int) (next []int, terminates bool) {
		routes, _ := steps[i]["routes"].(map[string]any)
		if len(routes) == 0 {
			// Sequential fallback: the next step, or the end of the workflow.
			if i+1 < len(steps) {
				return []int{i + 1}, false
			}
			return nil, true
		}
		for _, raw := range routes {
			target, _ := raw.(string)
			if target == "" {
				continue
			}
			if strings.HasPrefix(target, "end-") {
				terminates = true
				continue
			}
			if idx, ok := indexByID[target]; ok {
				next = append(next, idx)
			}
		}
		return next, terminates
	}

	// Reachability + termination in one walk from the entry step (index 0,
	// the step StartWorkflow activates first).
	reachable := make([]bool, len(steps))
	canTerminate := false
	queue := []int{0}
	reachable[0] = true
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		next, terminates := successors(i)
		if terminates {
			canTerminate = true
		}
		for _, n := range next {
			if !reachable[n] {
				reachable[n] = true
				queue = append(queue, n)
			}
		}
	}

	// A route back to an earlier step is a rework loop: the engine
	// re-activates that step and resets what follows it. A loop is fine
	// when a person sits on it (a task or approval decides whether to go
	// round again); a loop made only of automatic steps (condition,
	// notification, join) would spin with nobody to stop it, so it is
	// refused here with the offending edge.
	stepLabel := func(i int) string {
		if name, _ := steps[i]["name"].(string); name != "" {
			return name
		}
		if id, _ := steps[i]["id"].(string); id != "" {
			return id
		}
		return fmt.Sprintf("step %d", i+1)
	}
	const (
		unvisited = 0
		onPath    = 1
		done      = 2
	)
	isHuman := func(i int) bool {
		t, _ := steps[i]["type"].(string)
		return t == "task" || t == "approval" || t == "1" || t == "2"
	}
	state := make([]int, len(steps))
	var loopEdges []string
	var path []int
	var visit func(i int)
	visit = func(i int) {
		state[i] = onPath
		path = append(path, i)
		next, _ := successors(i)
		for _, n := range next {
			switch state[n] {
			case onPath:
				// The cycle is the tail of the path from n back to i.
				human := false
				for k := len(path) - 1; k >= 0; k-- {
					if isHuman(path[k]) {
						human = true
					}
					if path[k] == n {
						break
					}
				}
				if !human {
					loopEdges = append(loopEdges, fmt.Sprintf("Route from %q back to %q creates a loop with no task or approval in it — nobody could ever stop it; put a task or approval on the loop, or route forward", stepLabel(i), stepLabel(n)))
				}
			case unvisited:
				visit(n)
			}
		}
		path = path[:len(path)-1]
		state[i] = done
	}
	for i := range steps {
		if state[i] == unvisited {
			visit(i)
		}
	}
	errs = append(errs, loopEdges...)

	for i, ok := range reachable {
		if ok {
			continue
		}
		label, _ := steps[i]["name"].(string)
		if label == "" {
			if id, _ := steps[i]["id"].(string); id != "" {
				label = id
			} else {
				label = fmt.Sprintf("step %d", i+1)
			}
		}
		errs = append(errs, fmt.Sprintf("Step %q is unreachable — no route from the first step leads to it", label))
	}

	if !canTerminate {
		errs = append(errs, "Workflow has no reachable end — every path loops without reaching an end route or a final step")
	}
	return errs
}
