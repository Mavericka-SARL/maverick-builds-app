package aiassistant

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
)

// The server edits a checked plan itself where the fix is certain, instead
// of sending it back for a turn: a failing layout step is dropped, a metric a
// plan leaves off every grid is placed on its reader's grid, and a plan that
// keeps failing is cut to the steps that pass (2026-11-11: a 38-step plan
// went back five times over one reorder step; the model, asked for the
// passing steps, re-sent the failing one each time).

// stepPlaceholder is a placeholder naming a step by number:
// "<created in step 3>", "<step 3 id>" — the form resolveParamRefs reads.
var stepPlaceholder = regexp.MustCompile(`(<[^"<>]*?(?i:step)\s*)(\d+)([^"<>]*>)`)

// renumberSteps rewrites every step-number placeholder in params through
// newNum (1-based old → 1-based new; 0 keeps the placeholder as written).
func renumberSteps(params json.RawMessage, newNum func(old int) int) json.RawMessage {
	out := stepPlaceholder.ReplaceAllStringFunc(string(params), func(m string) string {
		parts := stepPlaceholder.FindStringSubmatch(m)
		old, err := strconv.Atoi(parts[2])
		if err != nil {
			return m
		}
		n := newNum(old)
		if n <= 0 {
			return m
		}
		return parts[1] + strconv.Itoa(n) + parts[3]
	})
	return json.RawMessage(out)
}

// StepNumbersUsed are the 1-based steps a step's params name by number.
func StepNumbersUsed(params json.RawMessage) []int {
	var out []int
	for _, m := range stepPlaceholder.FindAllStringSubmatch(string(params), -1) {
		if n, err := strconv.Atoi(m[2]); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// DropSteps returns steps without the 0-based indexes in drop, with every
// "<… step N …>" reference renumbered to the step's new place. A step that
// names a dropped step by number is dropped too (it could not run), and the
// returned set holds every index dropped.
func DropSteps(steps []ProposalStep, drop map[int]bool) ([]ProposalStep, map[int]bool) {
	gone := map[int]bool{}
	for i := range drop {
		gone[i] = true
	}
	for changed := true; changed; {
		changed = false
		for i, s := range steps {
			if gone[i] {
				continue
			}
			for _, n := range StepNumbersUsed(s.Params) {
				if gone[n-1] {
					gone[i] = true
					changed = true
					break
				}
			}
		}
	}
	newNum := make([]int, len(steps)+1) // old 1-based → new 1-based
	kept := 0
	for i := range steps {
		if !gone[i] {
			kept++
			newNum[i+1] = kept
		}
	}
	out := make([]ProposalStep, 0, kept)
	for i, s := range steps {
		if gone[i] {
			continue
		}
		s.Params = renumberSteps(s.Params, func(old int) int {
			if old < 1 || old > len(steps) {
				return 0
			}
			return newNum[old]
		})
		out = append(out, s)
	}
	return out, gone
}

// InsertedStep is a step the server adds to a plan, placed right after the
// 0-based step After.
type InsertedStep struct {
	After int
	Step  ProposalStep
}

// InsertSteps adds steps to a plan, each right after its step, with every
// "<… step N …>" reference renumbered to the steps' new places.
func InsertSteps(steps []ProposalStep, add []InsertedStep) []ProposalStep {
	if len(add) == 0 {
		return steps
	}
	sort.SliceStable(add, func(a, b int) bool { return add[a].After < add[b].After })
	newNum := make([]int, len(steps)+1)
	shift, k := 0, 0
	for i := range steps {
		newNum[i+1] = i + 1 + shift
		for k < len(add) && add[k].After == i {
			shift++
			k++
		}
	}
	renum := func(old int) int {
		if old < 1 || old > len(steps) {
			return 0
		}
		return newNum[old]
	}
	out := make([]ProposalStep, 0, len(steps)+len(add))
	k = 0
	for i, s := range steps {
		s.Params = renumberSteps(s.Params, renum)
		out = append(out, s)
		for k < len(add) && add[k].After == i {
			a := add[k].Step
			a.Params = renumberSteps(a.Params, renum)
			out = append(out, a)
			k++
		}
	}
	for ; k < len(add); k++ { // After past the end: at the end
		out = append(out, add[k].Step)
	}
	return out
}

// PresentationTool reports a step that only arranges or labels what other
// steps make — an order, tags, a date format, the draft's name. Nothing reads
// its result, so when it fails the server drops it with a note instead of
// refusing the plan around it.
func PresentationTool(tool string) bool {
	switch tool {
	case "reorder_grid_metrics", "reorder_dimension_members", "reorder_dashboards", "set_tags", "rename_draft", "set_date_format":
		return true
	}
	return false
}
