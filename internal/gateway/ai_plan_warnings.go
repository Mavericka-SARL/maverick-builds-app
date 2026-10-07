package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// planWarnings are the plan check's warnings: formulas a plan writes that
// run, but look like the AI Developer's typical errors — a Percentage metric
// multiplied in without dividing by 100, and SUMIFS(src, D, D) where the
// total over the cell's other dimension is a LOOKUP. Read on the dry run's
// transaction once the whole plan has run, so a metric, its format and its
// grid are as the plan leaves them.
func planWarnings(ctx context.Context, tx pgx.Tx, modelID, revID string, steps []aiassistant.ProposalStep) []string {
	pct := map[string]bool{}
	if rows, err := tx.Query(ctx, `SELECT lower(name) FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND format='percentage'`, modelID, revID); err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				pct[n] = true
			}
		}
		rows.Close()
	}
	var out []string
	out = append(out, recurringMistakeWarnings(ctx, tx, modelID, revID, steps)...)
	out = append(out, hiddenDecimalWarnings(ctx, tx, modelID, revID, steps)...)
	for i, step := range steps {
		if step.Tool != "create_metric" && step.Tool != "update_metric" {
			continue
		}
		var p struct {
			Name     string `json:"name"`
			MetricID string `json:"metric_id"`
			Formula  string `json:"formula"`
		}
		if json.Unmarshal(step.Params, &p) != nil || strings.TrimSpace(p.Formula) == "" {
			continue
		}
		ref := p.Name
		if step.Tool == "update_metric" {
			ref = p.MetricID
		}
		var metricID, name, format string
		if tx.QueryRow(ctx, `
			SELECT id::text, name, COALESCE(format,'number') FROM model.metric_def
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND (id::text=$3 OR lower(name)=lower($3) OR lower(name)=lower($4))
			ORDER BY (id::text=$3) DESC LIMIT 1`, modelID, revID, ref, p.Name).Scan(&metricID, &name, &format) != nil {
			continue
		}
		at := fmt.Sprintf("step %d (%s %s)", i+1, step.Tool, name)
		if m := aiassistant.PercentFactorMisuse(p.Formula, pct, format == "percentage"); m != "" {
			out = append(out, fmt.Sprintf("%s: the formula multiplies %s, a Percentage metric stored in percent units (6 for 6%%), without dividing by 100 — write %s / 100 (or make %s a Percentage metric if it is one)",
				at, m, m, name))
		}
		for _, sum := range aiassistant.SelfCriteriaSums(p.Formula) {
			if w := selfSumWarning(ctx, tx, modelID, revID, metricID, sum); w != "" {
				out = append(out, at+": "+w)
			}
		}
	}
	return out
}

// hiddenDecimalWarnings names each metric a write_input_values step writes
// values with decimals into while it shows no decimal places (as the plan
// leaves it): 0.3 (USD millions) would read 0 and a 5.6% rate 6%.
func hiddenDecimalWarnings(ctx context.Context, tx pgx.Tx, modelID, revID string, steps []aiassistant.ProposalStep) []string {
	var out []string
	warned := map[string]bool{}
	for i, step := range steps {
		if step.Tool != "write_input_values" {
			continue
		}
		var p struct {
			MetricID string `json:"metric_id"`
			Values   []struct {
				Value any `json:"value"`
			} `json:"values"`
		}
		if json.Unmarshal(step.Params, &p) != nil {
			continue
		}
		frac, found := 0.0, false
		for _, v := range p.Values {
			var f float64
			switch x := v.Value.(type) {
			case float64:
				f = x
			case string:
				n, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
				if err != nil {
					continue
				}
				f = n
			default:
				continue
			}
			if f != math.Trunc(f) {
				frac, found = f, true
				break
			}
		}
		if !found {
			continue
		}
		var name, format string
		var decimals int
		if tx.QueryRow(ctx, `
			SELECT name, COALESCE(format,'number'), COALESCE(format_decimals,0) FROM model.metric_def
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND (id::text=$3 OR lower(name)=lower($3))
			ORDER BY (id::text=$3) DESC LIMIT 1`, modelID, revID, p.MetricID).Scan(&name, &format, &decimals) != nil {
			continue
		}
		if decimals != 0 || warned[name] || (format != "number" && format != "currency" && format != "percentage") {
			continue
		}
		warned[name] = true
		out = append(out, fmt.Sprintf("step %d (write_input_values %s): %s shows no decimal places, but the values have decimals (%s would show as %s): "+
			"set its format_decimals to the source's precision",
			i+1, name, name, strconv.FormatFloat(frac, 'f', -1, 64), strconv.FormatFloat(math.Round(frac), 'f', -1, 64)))
	}
	return out
}

// selfSumWarning words a SelfCriteriaSum on a metric whose grid carries every
// dimension of the source: the sum is the cell's own value, and the total
// over the dimensions the criteria leave out is a LOOKUP of their total
// member. "" when the source has a dimension the metric's grid lacks (then
// the sum does add its members up).
func selfSumWarning(ctx context.Context, tx pgx.Tx, modelID, revID, metricID string, sum aiassistant.SelfCriteriaSum) string {
	type dim struct{ id, name string }
	gridDims := func(metricRef string) ([]dim, bool) {
		rows, err := tx.Query(ctx, `
			SELECT d.id::text, d.name FROM model.metric_def m
			JOIN model.grid_metric gm ON gm.metric_id = m.id
			JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
			JOIN model.dimension_def d ON d.id = gd.dimension_id
			WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid AND (m.id::text=$3 OR lower(m.name)=lower($3))
			ORDER BY d.name`, modelID, revID, metricRef)
		if err != nil {
			return nil, false
		}
		defer rows.Close()
		var ds []dim
		for rows.Next() {
			var d dim
			if rows.Scan(&d.id, &d.name) == nil {
				ds = append(ds, d)
			}
		}
		return ds, len(ds) > 0
	}
	own, ok := gridDims(metricID)
	if !ok {
		return ""
	}
	src, ok := gridDims(sum.Source)
	if !ok {
		return ""
	}
	onOwn := map[string]bool{}
	for _, d := range own {
		onOwn[d.id] = true
	}
	criteria := map[string]bool{}
	for _, c := range sum.Dims {
		criteria[strings.ToLower(c)] = true
	}
	var rest []string
	for _, d := range src {
		if !onOwn[d.id] {
			return ""
		}
		if !criteria[strings.ToLower(d.name)] {
			var root string
			var roots int
			_ = tx.QueryRow(ctx, `SELECT COUNT(*), COALESCE(MIN(code),'') FROM model.dimension_member WHERE dimension_id=$1::uuid AND parent_member_id IS NULL`, d.id).Scan(&roots, &root)
			if roots == 1 {
				rest = append(rest, fmt.Sprintf("%s, %q", d.name, root))
			} else {
				rest = append(rest, fmt.Sprintf("%s, \"<its total member>\"", d.name))
			}
		}
	}
	if len(rest) == 0 {
		return fmt.Sprintf("%s(%s, %s, %s) compares %s with itself on a cell that carries every dimension of %s: it is the cell's own value — say what it should add up",
			sum.Call, sum.Source, sum.Dims[0], sum.Dims[0], sum.Dims[0], sum.Source)
	}
	return fmt.Sprintf("%s(%s, %s, %s, ...) keeps the cell's own member of every other dimension too, so it is the cell's own value, not a total: the %s's total is LOOKUP(%s, %s)",
		sum.Call, sum.Source, sum.Dims[0], sum.Dims[0], strings.Join(sum.Dims, "/"), sum.Source, strings.Join(rest, ", "))
}

// recurringMistakeWarnings are the AI Developer's mistakes seen rebuilding an
// HR planning workbook (2026-10-05), each a plan that runs but is not what
// was meant: an input typed full of 1s to count rows with SUMIFS (a row added
// later counts only once someone types its 1); a headcount summed over the
// months of a time grid (its FY shows twelve months added up); a summary
// grid widget showing every metric of a large grid.
func recurringMistakeWarnings(ctx context.Context, tx pgx.Tx, modelID, revID string, steps []aiassistant.ProposalStep) []string {
	var out []string
	levels := map[string]bool{}
	for i, step := range steps {
		at := fmt.Sprintf("step %d (%s)", i+1, step.Tool)
		switch step.Tool {
		case "write_input_values":
			var p struct {
				MetricID string `json:"metric_id"`
				Values   []struct {
					Value json.RawMessage `json:"value"`
				} `json:"values"`
			}
			if json.Unmarshal(step.Params, &p) != nil || len(p.Values) < 5 {
				continue
			}
			ones := true
			for _, v := range p.Values {
				if strings.TrimSpace(string(v.Value)) != "1" {
					ones = false
					break
				}
			}
			if ones {
				out = append(out, fmt.Sprintf("%s: every one of the %d values of %s is 1 — if it counts rows, count them with COUNTIFS(<pick-list or dimension>, <criterion>) instead: a typed 1 has to be typed again for every row added later",
					at, len(p.Values), p.MetricID))
			}
		case "create_metric", "update_metric", "add_grid_metric":
			var p struct {
				Name     string `json:"name"`
				MetricID string `json:"metric_id"`
			}
			if json.Unmarshal(step.Params, &p) == nil {
				levels[strings.ToLower(cmp.Or(p.MetricID, p.Name))] = true
			}
		case "create_grid":
			var p struct {
				Metrics   []string `json:"metrics"`
				MetricIDs []string `json:"metric_ids"`
			}
			if json.Unmarshal(step.Params, &p) == nil {
				for _, m := range append(p.Metrics, p.MetricIDs...) {
					levels[strings.ToLower(m)] = true
				}
			}
		case "add_dashboard_widget":
			var p struct {
				WidgetType  string          `json:"widget_type"`
				RefID       string          `json:"ref_id"`
				Title       string          `json:"title"`
				WidgetProps json.RawMessage `json:"widget_props"`
			}
			if json.Unmarshal(step.Params, &p) != nil || p.WidgetType != "grid" || p.RefID == "" {
				continue
			}
			var props struct {
				MetricIDs []string `json:"metric_ids"`
			}
			_ = json.Unmarshal(p.WidgetProps, &props)
			if len(props.MetricIDs) > 0 {
				continue
			}
			var gridName string
			var metrics int
			if tx.QueryRow(ctx, `
				SELECT g.name, (SELECT count(*) FROM model.grid_metric gm WHERE gm.grid_id = g.id)
				FROM model.grid_def g WHERE g.model_id=$1::uuid AND g.revision_id=$2::uuid AND (g.id::text=$3 OR lower(g.name)=lower($3))`,
				modelID, revID, p.RefID).Scan(&gridName, &metrics) != nil {
				continue
			}
			if metrics > 12 && p.Title != "" && !strings.EqualFold(strings.TrimSpace(p.Title), gridName) {
				out = append(out, fmt.Sprintf("%s: the grid widget %q shows all %d metrics of %s — a summary lists the ones it shows in widget_props.metric_ids",
					at, p.Title, metrics, gridName))
			}
		}
	}
	for ref := range levels {
		var name, label, timeSummary, format string
		var onTimeGrid bool
		if tx.QueryRow(ctx, `
			SELECT m.name, COALESCE(m.label,''), COALESCE(m.time_summary,'sum'), COALESCE(m.format,'number'),
			       EXISTS (SELECT 1 FROM model.grid_metric gm JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
			               JOIN model.dimension_def d ON d.id = gd.dimension_id
			               WHERE gm.metric_id = m.id AND d.dimension_type = 'time')
			FROM model.metric_def m
			WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid AND (m.id::text=$3 OR lower(m.name)=$3)`,
			modelID, revID, ref).Scan(&name, &label, &timeSummary, &format, &onTimeGrid) != nil {
			continue
		}
		if onTimeGrid && timeSummary == "sum" && format != "percentage" && looksLikeLevel(name, label) {
			out = append(out, fmt.Sprintf("%s looks like a level (a headcount or a balance) and is summed over the months of its time grid: its FY is the months added up — set time_summary \"last\" (closing), \"first\" (opening) or \"average\"", name))
		}
	}
	return out
}

// looksLikeLevel: a name or label word naming a stock rather than a flow.
func looksLikeLevel(name, label string) bool {
	words := strings.FieldsFunc(strings.ToLower(name+" "+label), func(r rune) bool {
		return r == '_' || r == ' ' || r == '-' || r == '.' || r == '(' || r == ')'
	})
	for _, w := range words {
		switch w {
		case "hc", "headcount", "fte", "ftes", "balance", "closing", "ending", "opening", "inventory", "stock":
			return true
		}
	}
	return false
}
