package aiassistant

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// OrderByDependencies returns a plan's steps with each metric created
// before the steps that name it: a formula's references, a grid's metric
// list, add_grid_metric, update_metric. A plan written in reading order
// ("Opening HC = …, Ending HC = opening_hc + hires_cum - dismissals_cum,
// Hires Cum. = …") failed its check on every forward reference, and the
// model, asked to reorder thirty steps, failed again five times.
//
// The order changes only where a step needs a later one: every other step
// keeps its place. Steps of any other kind are fixed points nothing moves
// across, a plan naming "<created in step N>" (positions) is left as it is,
// and so is one whose references form a cycle — its check reports that.
func OrderByDependencies(steps []ProposalStep) []ProposalStep {
	for _, s := range steps {
		if strings.Contains(string(s.Params), "created in step") { // "<…>", or JSON-escaped
			return steps
		}
	}
	n := len(steps)
	creator := map[string]int{} // lower-cased metric/grid name -> creating step
	needs := make([][]string, n)
	movable := make([]bool, n)
	for i, s := range steps {
		var p struct {
			Name      string   `json:"name"`
			Formula   string   `json:"formula"`
			MetricID  string   `json:"metric_id"`
			GridID    string   `json:"grid_id"`
			Metrics   []string `json:"metrics"`
			MetricIDs []string `json:"metric_ids"`
		}
		if json.Unmarshal(s.Params, &p) != nil {
			continue
		}
		switch s.Tool {
		case "create_metric", "create_grid":
			movable[i] = true
			if name := strings.ToLower(strings.TrimSpace(p.Name)); name != "" {
				if _, dup := creator[name]; !dup {
					creator[name] = i
				}
			}
		case "update_metric", "add_grid_metric", "reorder_grid_metrics":
			movable[i] = true
		}
		if !movable[i] {
			continue
		}
		if p.Formula != "" {
			if refs, err := formula.ExtractRefs(p.Formula); err == nil {
				needs[i] = append(needs[i], refs...)
			}
		}
		needs[i] = append(needs[i], p.Metrics...)
		needs[i] = append(needs[i], p.MetricIDs...)
		needs[i] = append(needs[i], p.MetricID, p.GridID)
	}
	// deps[i]: the steps i must follow — the creators of what it names, and
	// the last fixed step before it (a fixed step follows everything before).
	deps := make([]map[int]bool, n)
	lastFixed := -1
	for i := range steps {
		deps[i] = map[int]bool{}
		if !movable[i] {
			for j := 0; j < i; j++ {
				deps[i][j] = true
			}
			lastFixed = i
			continue
		}
		if lastFixed >= 0 {
			deps[i][lastFixed] = true
		}
		for _, name := range needs[i] {
			if c, ok := creator[strings.ToLower(strings.TrimSpace(name))]; ok && c != i {
				deps[i][c] = true
			}
		}
	}
	// Kahn's algorithm, always taking the earliest ready step: the stable
	// topological order.
	done := make([]bool, n)
	out := make([]ProposalStep, 0, n)
	for len(out) < n {
		var ready []int
		for i := range steps {
			if done[i] {
				continue
			}
			ok := true
			for d := range deps[i] {
				if !done[d] {
					ok = false
					break
				}
			}
			if ok {
				ready = append(ready, i)
			}
		}
		if len(ready) == 0 {
			return steps // a cycle: left for the check to report
		}
		sort.Ints(ready)
		done[ready[0]] = true
		out = append(out, steps[ready[0]])
	}
	return out
}
