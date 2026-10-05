package aiassistant

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// LintProposalStep holds a plan step to what the dry run cannot see: a step
// that would save cleanly and compute the wrong thing. The AI Developer's
// plans passed the dry run with six P&L lines written as the formula "0",
// and margins and variance percentages totalled by average. These are
// refusals for the assistant only — a developer in the console can still
// save any of them on purpose.
func LintProposalStep(tool string, params json.RawMessage) error {
	if err := unknownParams(tool, params); err != nil {
		return err
	}
	if tool != "create_metric" && tool != "update_metric" {
		return nil
	}
	var p struct {
		Name    string  `json:"name"`
		Formula string  `json:"formula"`
		IsInput *bool   `json:"is_input"`
		AggRule *string `json:"agg_rule"`
		Format  string  `json:"format"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p.Formula), "="))
	calculated := text != "" && (p.IsInput == nil || !*p.IsInput)
	if tool == "create_metric" && p.IsInput != nil && *p.IsInput && p.Format == "percentage" &&
		(p.AggRule == nil || *p.AggRule == "" || *p.AggRule == "sum") {
		return fmt.Errorf("metric %q is a percentage input totalled by sum: a year or a region would show the percentages added up. "+
			"Use agg_rule \"average\" (an input has no formula to re-evaluate)", p.Name)
	}
	if !calculated {
		return nil
	}
	node, parseErr := formula.Parse(text)
	if parseErr == nil {
		if call := singleMetricAggregate(node); call != "" {
			return fmt.Errorf("metric %q: %s of a single metric is that metric itself — a formula is evaluated at each cell, not over the model. "+
				"A total over a dimension is a metric on a grid without that dimension (it reads the other metric's total), "+
				"or LOOKUP(metric, dimension, \"<a parent member>\"); a total by a property is SUMIFS(source, {Dimension}.property, \"value\")",
				p.Name, call)
		}
	}
	if parseErr == nil && literalOnly(node) {
		return fmt.Errorf("metric %q: the formula %q reads nothing, so every cell is that constant — a placeholder, not a calculation. "+
			"Write the real formula (a total of other metrics by a property is SUMIFS(source, {Dimension}.property, \"value\")), "+
			"or ask the developer how this line is computed", p.Name, p.Formula)
	}
	if tool != "create_metric" || p.Format != "percentage" {
		return nil
	}
	agg := ""
	if p.AggRule != nil {
		agg = *p.AggRule
	}
	if agg == "" || agg == "sum" || agg == "average" {
		shown := agg
		if shown == "" {
			shown = "sum (the default)"
		}
		return fmt.Errorf("metric %q is a calculated percentage with agg_rule %s: its total would add or average the members' percentages. "+
			"Use agg_rule \"formula\" so a total is the formula on the totals (total margin / total revenue)", p.Name, shown)
	}
	if strings.Contains(text, "/") && !strings.Contains(text, "100") {
		return fmt.Errorf("metric %q: a Percentage metric stores percent units (68 shows as 68%%), but %q is a fraction (0.68 would show as 0.68%%). "+
			"Multiply the ratio by 100", p.Name, p.Formula)
	}
	return nil
}

// literalOnly reports a formula with no name and no function call in it:
// numbers, text and arithmetic on them only.
func literalOnly(n formula.Node) bool {
	switch v := n.(type) {
	case *formula.NumberLit, *formula.StringLit, *formula.BoolLit:
		return true
	case *formula.UnaryExpr:
		return literalOnly(v.Expr)
	case *formula.BinaryExpr:
		return literalOnly(v.Left) && literalOnly(v.Right)
	}
	return false
}

// singleMetricAggregate finds SUM(x), AVERAGE(x), MIN(x), MAX(x) or COUNT(x)
// over one bare name — the assistant wrote SUM(rolling_revenue_forecast)
// for a model-wide total — and returns how it was written, or "".
func singleMetricAggregate(n formula.Node) string {
	switch v := n.(type) {
	case *formula.CallExpr:
		switch strings.ToUpper(v.Name) {
		case "SUM", "AVERAGE", "MIN", "MAX", "COUNT":
			if len(v.Args) == 1 {
				if id, ok := v.Args[0].(*formula.Ident); ok {
					return fmt.Sprintf("%s(%s)", strings.ToUpper(v.Name), id.Name)
				}
			}
		}
		for _, a := range v.Args {
			if s := singleMetricAggregate(a); s != "" {
				return s
			}
		}
	case *formula.BinaryExpr:
		if s := singleMetricAggregate(v.Left); s != "" {
			return s
		}
		return singleMetricAggregate(v.Right)
	case *formula.UnaryExpr:
		return singleMetricAggregate(v.Expr)
	}
	return ""
}

// PercentUnitsMisuse finds "1 + p" or "1 - p" where p is named in
// percentMetrics (the revision's Percentage metrics, stored in percent units
// — 5.6 for 5.6%), and returns p, or "". Live, the assistant wrote
// prior_year_revenue * (1 + revenue_planning_pct) over values such as 5.6.
func PercentUnitsMisuse(text string, percentMetrics map[string]bool) string {
	node, err := formula.Parse(strings.TrimPrefix(strings.TrimSpace(text), "="))
	if err != nil {
		return ""
	}
	var walk func(n formula.Node) string
	walk = func(n formula.Node) string {
		switch v := n.(type) {
		case *formula.BinaryExpr:
			if v.Op == "+" || v.Op == "-" {
				for _, pair := range [][2]formula.Node{{v.Left, v.Right}, {v.Right, v.Left}} {
					if num, ok := pair[0].(*formula.NumberLit); ok && num.Val == 1 {
						if id, ok := pair[1].(*formula.Ident); ok && percentMetrics[strings.ToLower(id.Name)] {
							return id.Name
						}
					}
				}
			}
			if s := walk(v.Left); s != "" {
				return s
			}
			return walk(v.Right)
		case *formula.UnaryExpr:
			return walk(v.Expr)
		case *formula.CallExpr:
			for _, a := range v.Args {
				if s := walk(a); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return walk(node)
}

// knownParams lists what the most used write tools read. A key outside it
// was dropped without a word: create_grid's "metrics" left every grid the
// assistant built empty, and create_metric's "value": 9 left a setting blank.
var knownParams = map[string][]string{
	"create_metric": {"name", "label", "formula", "is_input", "format", "format_decimals", "format_currency", "agg_rule", "revision_id",
		"agg_numerator_metric_id", "agg_denominator_metric_id", "time_summary", "tags", "picklist_dimension"},
	"create_grid": {"name", "revision_id", "metric_ids", "dimension_ids", "metrics", "dimensions"},
}

func unknownParams(tool string, params json.RawMessage) error {
	known, ok := knownParams[tool]
	if !ok {
		return nil
	}
	var p map[string]json.RawMessage
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	set := map[string]bool{}
	for _, k := range known {
		set[k] = true
	}
	var unknown []string
	for k := range p {
		if !set[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	if tool == "create_metric" && (p["value"] != nil || p["initial_value"] != nil || p["default_value"] != nil) {
		return fmt.Errorf("create_metric has no value: an input's values are typed by people in a grid or imported (import_file_data) — "+
			"leave it out and tell the developer which value to enter (it reads: %s)", strings.Join(known, ", "))
	}
	return fmt.Errorf("%s does not read %s — it reads: %s", tool, strings.Join(unknown, ", "), strings.Join(known, ", "))
}
