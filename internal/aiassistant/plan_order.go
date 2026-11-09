package aiassistant

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
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
	steps = NamePlaceholders(steps)
	for _, s := range steps {
		if strings.Contains(string(s.Params), "created in step") { // "<…>", or JSON-escaped
			return steps
		}
	}
	steps = splitMetricCycles(steps)
	n := len(steps)
	creator := map[string]int{} // lower-cased metric/grid name -> creating step
	needs := make([][]string, n)
	movable := make([]bool, n)
	formulaSetters := map[int][]string{} // update_metric steps setting a formula -> the metrics it and its formula name
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
		if s.Tool == "update_metric" && p.Formula != "" {
			formulaSetters[i] = append([]string{p.MetricID}, needs[i]...)
		}
	}
	// A formula set by update_metric — the second half of a cycle the plan
	// split, or a formula carried forward with LAG — follows the steps that
	// place it and the metrics it names on their grids: a cycle broken by
	// time is accepted only once its metrics are on a time grid
	// (TIME_DIMENSION_REQUIRED), and the AI Developer placed them last.
	placers := map[string][]int{}
	for i, s := range steps {
		var p struct {
			MetricID string   `json:"metric_id"`
			Metrics  []string `json:"metrics"`
		}
		if (s.Tool != "add_grid_metric" && s.Tool != "create_grid") || json.Unmarshal(s.Params, &p) != nil {
			continue
		}
		for _, m := range append(p.Metrics, p.MetricID) {
			if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
				placers[m] = append(placers[m], i)
			}
		}
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
		for _, name := range formulaSetters[i] {
			for _, pl := range placers[strings.ToLower(strings.TrimSpace(name))] {
				if pl != i && movable[pl] {
					deps[i][pl] = true
				}
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

// splitMetricCycles lets a plan create metrics whose formulas name each
// other — a carried-forward pair, opening = LAG(closing) and closing =
// opening + hires - leavers: neither can be created first, since a formula
// may name only metrics that exist, so the plan check refused it and the
// AI Developer needed a second proposal to fill in placeholders
// (2026-10-20). Each metric of such a cycle is created with the formula 0,
// and an update_metric step setting its formula follows once every metric
// it names exists (OrderByDependencies places it). A cycle time does not
// break is still refused, at that step, by the engine's own check.
func splitMetricCycles(steps []ProposalStep) []ProposalStep {
	type metric struct {
		name    string
		formula string
		refs    []string
	}
	creator := map[string]int{}
	metrics := map[int]metric{}
	for i, s := range steps {
		if s.Tool != "create_metric" {
			continue
		}
		var p map[string]any
		if json.Unmarshal(s.Params, &p) != nil {
			continue
		}
		name, _ := p["name"].(string)
		f, _ := p["formula"].(string)
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || strings.TrimSpace(f) == "" {
			continue
		}
		refs, err := formula.ExtractRefs(f)
		if err != nil {
			continue
		}
		if _, dup := creator[name]; !dup {
			creator[name] = i
		}
		metrics[i] = metric{name: name, formula: f, refs: refs}
	}
	// reaches[i][j]: i's formula names j, directly or through others.
	reaches := map[int]map[int]bool{}
	for i, m := range metrics {
		reaches[i] = map[int]bool{}
		for _, r := range m.refs {
			if j, ok := creator[strings.ToLower(strings.TrimSpace(r))]; ok && j != i {
				reaches[i][j] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for i := range reaches {
			for j := range reaches[i] {
				for k := range reaches[j] {
					if !reaches[i][k] {
						reaches[i][k], changed = true, true
					}
				}
			}
		}
	}
	var out []ProposalStep
	for i, s := range steps {
		m, ok := metrics[i]
		if !ok || !reaches[i][i] {
			out = append(out, s)
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(s.Params, &p)
		p["formula"] = "0"
		created, _ := json.Marshal(p)
		update, _ := json.Marshal(map[string]any{"metric_id": p["name"], "formula": m.formula})
		out = append(out, ProposalStep{Tool: s.Tool, Description: s.Description + " (its formula follows once the metrics it reads exist)", Params: created},
			ProposalStep{Tool: "update_metric", Description: "Set the formula of '" + m.name + "': " + m.formula, Params: update})
	}
	return out
}

// placeholderStep matches a whole "<created in step N>" value.
var placeholderStep = regexp.MustCompile(`^<created in step (\d+)>$`)

// placeholderKinds is what a param names, by key, among the params that take
// a name as well as an id: a placeholder there stands for an object of that
// kind. (agg_numerator_metric_id and its pair take ids only: a placeholder
// there stays, and the plan keeps its positions.)
var placeholderKinds = map[string]string{
	"metric_id": "metric", "metrics": "metric", "metric_ids": "metric",
	"grid_id": "grid", "dimension_id": "dimension", "dimensions": "dimension",
	"dashboard_id": "dashboard",
}

// creatorKinds is what a creating tool makes, named by its "name".
var creatorKinds = map[string]string{
	"create_metric": "metric", "create_grid": "grid", "create_dimension": "dimension", "create_dashboard": "dashboard",
}

// NamePlaceholders writes each "<created in step N>" that stands for a
// metric, grid, dimension or page the plan creates as that object's name,
// which every tool takes as well as an id. A step position is fragile:
// gpt-5-mini pointed every add_grid_metric of a plan at its own step number
// and was refused six times running for it (2026-11-08), and a plan using
// positions could not be put in dependency order at all. A placeholder
// pointing at a step that makes no such object is read as the one object of
// that kind the plan creates and the step's description names; otherwise it
// is left for the check to report.
func NamePlaceholders(steps []ProposalStep) []ProposalStep {
	type made struct{ kind, name string }
	created := make([]made, len(steps))
	// about: the metric a step that creates nothing works on, by name — a
	// placeholder pointing at "update opening_members" means that metric
	// (gpt-5-mini listed a grid's metrics that way, 2026-11-09).
	about := make([]made, len(steps))
	for i, s := range steps {
		var p struct {
			Name     string `json:"name"`
			MetricID string `json:"metric_id"`
		}
		if json.Unmarshal(s.Params, &p) != nil {
			continue
		}
		if kind := creatorKinds[s.Tool]; kind != "" && strings.TrimSpace(p.Name) != "" {
			created[i] = made{kind, strings.TrimSpace(p.Name)}
			continue
		}
		if m := strings.TrimSpace(p.MetricID); m != "" && !strings.Contains(m, "<") && (s.Tool == "update_metric" || s.Tool == "add_grid_metric" || s.Tool == "write_input_values") {
			about[i] = made{"metric", m}
		}
	}
	resolve := func(i int, kind, v string) (string, bool) {
		m := placeholderStep.FindStringSubmatch(strings.TrimSpace(v))
		if m == nil {
			return v, false
		}
		n, _ := strconv.Atoi(m[1])
		if n >= 1 && n <= len(steps) && created[n-1].kind == kind {
			return created[n-1].name, true
		}
		if n >= 1 && n <= len(steps) && about[n-1].kind == kind {
			return about[n-1].name, true
		}
		desc := strings.ToLower(steps[i].Description)
		found := ""
		for j, c := range created {
			if j == i || c.kind != kind || !strings.Contains(desc, strings.ToLower(c.name)) {
				continue
			}
			if found != "" && !strings.EqualFold(found, c.name) {
				return v, false // two candidates: ambiguous
			}
			found = c.name
		}
		if found == "" {
			return v, false
		}
		return found, true
	}
	out := make([]ProposalStep, len(steps))
	for i, s := range steps {
		out[i] = s
		if !strings.Contains(string(s.Params), "created in step") {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(string(s.Params)))
		dec.UseNumber()
		var p map[string]any
		if dec.Decode(&p) != nil {
			continue
		}
		changed := false
		for key, kind := range placeholderKinds {
			switch v := p[key].(type) {
			case string:
				if name, ok := resolve(i, kind, v); ok {
					p[key], changed = name, true
				}
			case []any:
				for k, e := range v {
					if str, isStr := e.(string); isStr {
						if name, ok := resolve(i, kind, str); ok {
							v[k], changed = name, true
						}
					}
				}
			}
		}
		if !changed {
			continue
		}
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if enc.Encode(p) == nil {
			out[i].Params = json.RawMessage(strings.TrimSpace(buf.String()))
		}
	}
	return out
}
