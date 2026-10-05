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

// Steps of other kinds are fixed points; a cycle or a positional reference
// leaves the plan as written.
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
	if got := toolsOf(OrderByDependencies(cycle)); got != "create_metric:a create_metric:b" {
		t.Errorf("a cycle was reordered: %s", got)
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
