package aiassistant

import (
	"encoding/json"
	"strings"
	"testing"
)

// What the AI Developer proposed live and the dry run let through: P&L lines
// written as "0", ratios totalled by average, a margin stored as a fraction.
func TestLintProposalStep(t *testing.T) {
	j := func(m map[string]any) json.RawMessage { b, _ := json.Marshal(m); return b }
	for _, tc := range []struct {
		what   string
		tool   string
		params map[string]any
		want   string // "" = allowed
	}{
		{"a placeholder line", "create_metric", map[string]any{"name": "marketing", "formula": "0", "is_input": false}, "reads nothing"},
		{"a placeholder on update", "update_metric", map[string]any{"metric_id": "m", "formula": "= 1 + 2"}, "reads nothing"},
		{"a margin averaged", "create_metric", map[string]any{"name": "m", "formula": "ebitda / revenue * 100", "format": "percentage", "agg_rule": "average"}, `agg_rule "formula"`},
		{"a margin summed by default", "create_metric", map[string]any{"name": "m", "formula": "ebitda / revenue * 100", "format": "percentage"}, `agg_rule "formula"`},
		{"a margin as a fraction", "create_metric", map[string]any{"name": "m", "formula": "ebitda / revenue", "format": "percentage", "agg_rule": "formula"}, "Multiply the ratio by 100"},
		{"a right margin", "create_metric", map[string]any{"name": "m", "formula": "IF(revenue = 0, 0, ebitda / revenue * 100)", "format": "percentage", "agg_rule": "formula"}, ""},
		{"a rate percentage", "create_metric", map[string]any{"name": "m", "formula": "a / b * 100", "format": "percentage", "agg_rule": "rate"}, ""},
		{"a forecast reading a percent", "create_metric", map[string]any{"name": "rf", "formula": "ly * (1 + pct / 100)", "format": "currency"}, ""},
		{"a period-dependent value", "create_metric", map[string]any{"name": "days", "formula": "DAYSINMONTH(YEAR(START()), MONTH(START()))"}, ""},
		{"an input percentage", "create_metric", map[string]any{"name": "pct", "is_input": true, "format": "percentage", "agg_rule": "average"}, ""},
		{"an input percentage summed", "create_metric", map[string]any{"name": "pct", "is_input": true, "format": "percentage", "agg_rule": "sum"}, `agg_rule "average"`},
		{"another tool", "create_grid", map[string]any{"name": "g"}, ""},
		{"a value on a metric", "create_metric", map[string]any{"name": "cutoff", "is_input": true, "value": 9}, "create_metric has no value"},
		{"a key create_grid does not read", "create_grid", map[string]any{"name": "g", "rows": []string{"a"}}, "create_grid does not read rows"},
	} {
		err := LintProposalStep(tc.tool, j(tc.params))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.what, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want %q", tc.what, err, tc.want)
		}
	}
}

func TestLintSingleMetricAggregate(t *testing.T) {
	j := func(m map[string]any) json.RawMessage { b, _ := json.Marshal(m); return b }
	if err := LintProposalStep("create_metric", j(map[string]any{"name": "share", "formula": "rf / SUM(rf) * 100"})); err == nil || !strings.Contains(err.Error(), "SUM(rf) of a single metric") {
		t.Errorf("SUM of one metric: %v", err)
	}
	if err := LintProposalStep("create_metric", j(map[string]any{"name": "total", "formula": "SUM(a, b)"})); err != nil {
		t.Errorf("SUM of two metrics refused: %v", err)
	}
}

// Over 5.6 for 5.6%, (1 + pct) multiplies by 6.6.
func TestPercentUnitsMisuse(t *testing.T) {
	pct := map[string]bool{"revenue_planning_pct": true}
	for text, want := range map[string]string{
		"ly * (1 + revenue_planning_pct)":                             "revenue_planning_pct",
		"ly * (Revenue_Planning_Pct + 1)":                             "Revenue_Planning_Pct",
		"ly * (1 - revenue_planning_pct)":                             "revenue_planning_pct",
		"ly * (1 + revenue_planning_pct / 100)":                       "",
		"ly * (1 + growth)":                                           "",
		"IF(MONTH(START()) <= 9, a, ly * (1 + revenue_planning_pct))": "revenue_planning_pct",
	} {
		if got := PercentUnitsMisuse(text, pct); got != want {
			t.Errorf("%s: got %q, want %q", text, got, want)
		}
	}
}
