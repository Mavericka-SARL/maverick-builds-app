package aiassistant

import (
	"encoding/json"
	"strings"
	"testing"
)

// What the AI Developer proposed live and the dry run let through: P&L lines
// written as "0", ratios totalled by average, a margin stored as a fraction,
// monthly planning rates added up into a year.
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
		{"an input percentage", "create_metric", map[string]any{"name": "pct", "is_input": true, "format": "percentage", "agg_rule": "average", "time_summary": "average"}, ""},
		{"a planning rate summed over the year", "create_metric", map[string]any{"name": "pct", "is_input": true, "format": "percentage", "agg_rule": "average"}, `time_summary "average"`},
		{"a planning rate summed over the year on purpose", "create_metric", map[string]any{"name": "pct", "is_input": true, "format": "percentage", "agg_rule": "average", "time_summary": "sum"}, `time_summary "average"`},
		{"a closing rate", "create_metric", map[string]any{"name": "pct", "is_input": true, "format": "percentage", "agg_rule": "none", "time_summary": "last"}, ""},
		{"a calculated rate with no total summed over time", "create_metric", map[string]any{"name": "eff", "formula": "pct", "format": "percentage", "agg_rule": "none"}, `time_summary "average"`},
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

// The AI Developer's two typical formula errors, found rebuilding a sales
// target-setting workbook: a Percentage metric multiplied in as if it were a
// fraction, and SUMIFS(src, D, D) for "the total of my region".
func TestPercentFactorMisuse(t *testing.T) {
	pct := map[string]bool{"growth_pct": true, "share_pct": true}
	for _, c := range []struct {
		formula string
		isPct   bool
		want    string
	}{
		{"ly_sales * growth_pct", false, "growth_pct"},
		{"ly_sales * growth_pct / 100", false, ""},
		{"ly_sales * (growth_pct / 100)", false, ""},
		{"ly_sales * growth_pct * 0.01", false, ""},
		{"ROUND(ly_sales * growth_pct, 2)", false, "growth_pct"},
		{"IF(x > 0, base * share_pct, 0)", false, "share_pct"},
		{"growth_pct * share", true, ""},               // percent × a fraction is a percent
		{"growth_pct * share_pct", true, "growth_pct"}, // percent × percent is not
		{"growth_pct * share_pct / 100", true, ""},
		{"var / base * 100", true, ""},
		{"growth_pct - share_pct", true, ""},
		{"growth_pct > 5", false, ""},
		{"base * (1 + growth_pct / 100)", false, ""},
		{"a / growth_pct", false, ""},
	} {
		if got := PercentFactorMisuse(c.formula, pct, c.isPct); got != c.want {
			t.Errorf("PercentFactorMisuse(%q, pct result %v) = %q, want %q", c.formula, c.isPct, got, c.want)
		}
	}
}

func TestSelfCriteriaSums(t *testing.T) {
	for _, c := range []struct {
		formula string
		want    string
	}{
		{"weighted_base / SUMIFS(weighted_base, Region, Region)", "SUMIFS weighted_base Region"},
		{"SUMIFS(x, region, region, product, PRODUCT)", "SUMIFS x region,product"},
		{"SUMIF(region, region, x)", "SUMIF x region"},
		{`SUMIFS(x, region, "NA")`, ""},
		{"SUMIFS(x, region, product)", ""},
		{"SUMIFS(x, region, region, product, \"SN\")", ""},
		{"LOOKUP(x, product, \"ALL\")", ""},
	} {
		var got []string
		for _, s := range SelfCriteriaSums(c.formula) {
			got = append(got, s.Call+" "+s.Source+" "+strings.Join(s.Dims, ","))
		}
		if strings.Join(got, ";") != c.want {
			t.Errorf("SelfCriteriaSums(%q) = %q, want %q", c.formula, got, c.want)
		}
	}
}
