package aiassistant

import (
	"encoding/json"
	"strings"
	"testing"
)

func step(tool string, params map[string]any) ProposalStep {
	raw, _ := json.Marshal(params)
	return ProposalStep{Tool: tool, Params: raw}
}

func toolsOf(steps []ProposalStep) string {
	var names []string
	for _, s := range steps {
		var p struct {
			Name     string `json:"name"`
			MetricID string `json:"metric_id"`
		}
		_ = json.Unmarshal(s.Params, &p)
		names = append(names, s.Tool+":"+p.Name+p.MetricID)
	}
	return strings.Join(names, " ")
}

// A plan in reading order runs in dependency order: each metric is created
// before the formulas, grids and attachments that name it; nothing else moves.
func TestOrderByDependenciesCreatesWhatIsNamedFirst(t *testing.T) {
	steps := []ProposalStep{
		step("create_metric", map[string]any{"name": "ending_hc", "formula": "opening_hc + hires_cum"}),
		step("create_metric", map[string]any{"name": "opening_hc", "formula": "dept_current_hc"}),
		step("create_grid", map[string]any{"name": "Plan", "metrics": []string{"ending_hc", "opening_hc", "hires_cum"}}),
		step("create_metric", map[string]any{"name": "hires_cum", "formula": `COUNTIFS(wa_type, "Hire")`}),
		step("add_grid_metric", map[string]any{"grid_id": "Plan", "metric_id": "final"}),
		step("create_metric", map[string]any{"name": "final", "formula": "ENDING_HC * 2"}),
	}
	got := toolsOf(OrderByDependencies(steps))
	want := "create_metric:opening_hc create_metric:hires_cum create_metric:ending_hc create_grid:Plan create_metric:final add_grid_metric:final"
	if got != want {
		t.Errorf("order:\n got %s\nwant %s", got, want)
	}
}

// Steps of other kinds are fixed points; a positional reference leaves the
// plan as written, and a cycle is created first and filled in after.
func TestOrderByDependenciesKeepsFixedStepsAndCycles(t *testing.T) {
	fixed := []ProposalStep{
		step("create_metric", map[string]any{"name": "a", "formula": "b"}),
		step("create_dimension", map[string]any{"name": "region"}),
		step("create_metric", map[string]any{"name": "b", "formula": "1"}),
	}
	if got := toolsOf(OrderByDependencies(fixed)); got != "create_metric:a create_dimension:region create_metric:b" {
		t.Errorf("a metric moved across a fixed step: %s", got)
	}
	cycle := []ProposalStep{
		step("create_metric", map[string]any{"name": "a", "formula": "b"}),
		step("create_metric", map[string]any{"name": "b", "formula": "a"}),
	}
	if got := toolsOf(OrderByDependencies(cycle)); got != "create_metric:a create_metric:b update_metric:a update_metric:b" {
		t.Errorf("a cycle: %s", got)
	}
	positional := []ProposalStep{
		step("add_grid_metric", map[string]any{"grid_id": "<created in step 2>", "metric_id": "x"}),
		step("create_grid", map[string]any{"name": "G"}),
		step("create_metric", map[string]any{"name": "x"}),
	}
	if got := toolsOf(OrderByDependencies(positional)); !strings.HasPrefix(got, "add_grid_metric") {
		t.Errorf("a plan with positional references was reordered: %s", got)
	}
}

// A carried-forward pair — each formula names the other, the cycle broken
// by LAG — is one plan: both metrics are created with the formula 0, then
// each formula is set once both exist. Other metrics keep their formulas.
func TestOrderByDependenciesSplitsACarriedForwardPair(t *testing.T) {
	steps := []ProposalStep{
		step("create_metric", map[string]any{"name": "cash_opening", "formula": "LAG(cash_closing, 1, 0)", "format": "currency"}),
		step("create_metric", map[string]any{"name": "cash_closing", "formula": "cash_opening + net_flow"}),
		step("create_metric", map[string]any{"name": "net_flow", "formula": "receipts - payments"}),
	}
	out := OrderByDependencies(steps)
	at := map[string]int{}
	for i, s := range out {
		var p map[string]any
		_ = json.Unmarshal(s.Params, &p)
		at[s.Tool+":"+fmt_(p["name"])+fmt_(p["metric_id"])] = i + 1
	}
	for later, earlier := range map[string][]string{
		"update_metric:cash_opening": {"create_metric:cash_opening", "create_metric:cash_closing"},
		"update_metric:cash_closing": {"create_metric:cash_closing", "create_metric:cash_opening", "create_metric:net_flow"},
	} {
		for _, e := range earlier {
			if at[later] == 0 || at[e] == 0 || at[later] < at[e] {
				t.Errorf("%s (step %d) is not after %s (step %d): %s", later, at[later], e, at[e], toolsOf(out))
			}
		}
	}
	formulas := map[string]string{}
	for _, s := range out {
		var p map[string]any
		_ = json.Unmarshal(s.Params, &p)
		key := s.Tool + ":" + fmt_(p["name"]) + fmt_(p["metric_id"])
		formulas[key] = fmt_(p["formula"])
		if s.Tool == "create_metric" && p["name"] == "cash_opening" && p["format"] != "currency" {
			t.Errorf("the created metric lost its other params: %v", p)
		}
	}
	for key, want := range map[string]string{
		"create_metric:cash_opening": "0", "create_metric:cash_closing": "0", "create_metric:net_flow": "receipts - payments",
		"update_metric:cash_opening": "LAG(cash_closing, 1, 0)", "update_metric:cash_closing": "cash_opening + net_flow",
	} {
		if formulas[key] != want {
			t.Errorf("%s formula %q, want %q", key, formulas[key], want)
		}
	}
}

func fmt_(v any) string {
	s, _ := v.(string)
	return s
}
