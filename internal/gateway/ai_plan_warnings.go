package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/modeledit"
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
	out = append(out, emptyInputWarnings(ctx, tx, modelID, revID, steps)...)
	out = append(out, memberlessDimensionWarnings(ctx, tx, revID, steps)...)
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
		out = append(out, emptyPropertyWarnings(ctx, tx, revID, at, p.Formula)...)
	}
	return out
}

// emptyPropertyWarnings names each dim.property the formula reads that no
// member of the dimension holds a value for, as the plan leaves it: the
// formula reads nothing in every cell. The AI build's 19 Actual P&L lines
// summed SUMIFS(…, {GL Account}.reporting_line, "Sponsorships"): the
// property was declared, every account came from a table keeping the
// dimension, none had a value, and every closed month read 0 with no
// warning (2026-11-08).
func emptyPropertyWarnings(ctx context.Context, tx pgx.Tx, revID, at, text string) []string {
	an, err := formula.Analyze(text)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, ref := range an.PropertyRefs {
		key := strings.ToLower(ref.Dim + "." + ref.Property)
		if seen[key] {
			continue
		}
		seen[key] = true
		var dimName, prop string
		var members, filled int
		if tx.QueryRow(ctx, `
			SELECT d.name, p.name,
			       (SELECT count(*) FROM model.dimension_member m WHERE m.dimension_id = d.id),
			       (SELECT count(*) FROM model.dimension_member m WHERE m.dimension_id = d.id
			         AND COALESCE(btrim(m.properties->>p.name), '') <> '')
			FROM model.dimension_def d JOIN model.dimension_property p ON p.dimension_id = d.id
			WHERE d.revision_id = $1::uuid AND lower(d.name) = lower($2) AND lower(p.name) = lower($3)`,
			revID, ref.Dim, ref.Property).Scan(&dimName, &prop, &members, &filled) != nil {
			continue
		}
		if members > 0 && filled == 0 {
			out = append(out, fmt.Sprintf("%s: the formula reads %s.%s, which none of %s's %d members has a value for, so it reads nothing in every cell — "+
				"give the members their values first (update_dimension_member, or the dimension's import), or read the mapping from where it is kept",
				at, ref.Dim, prop, dimName, members))
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
				var name, label string
				_ = tx.QueryRow(ctx, `
					SELECT name, COALESCE(label,'') FROM model.metric_def
					WHERE model_id=$1::uuid AND revision_id=$2::uuid AND (id::text=$3 OR lower(name)=lower($3))`,
					modelID, revID, p.MetricID).Scan(&name, &label)
				ones = looksLikeCount(cmp.Or(name, p.MetricID) + " " + label)
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
		var name, label, timeSummary, format, aggRule string
		var onTimeGrid bool
		if tx.QueryRow(ctx, `
			SELECT m.name, COALESCE(m.label,''), COALESCE(m.time_summary,'sum'), COALESCE(m.format,'number'), COALESCE(m.agg_rule,'sum'),
			       EXISTS (SELECT 1 FROM model.grid_metric gm JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
			               JOIN model.dimension_def d ON d.id = gd.dimension_id
			               WHERE gm.metric_id = m.id AND d.dimension_type = 'time')
			FROM model.metric_def m
			WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid AND (m.id::text=$3 OR lower(m.name)=$3)`,
			modelID, revID, ref).Scan(&name, &label, &timeSummary, &format, &aggRule, &onTimeGrid) != nil {
			continue
		}
		// A total by formula is the formula on the totals, over time too: a
		// time summary other than sum never applies. "Peak capacity usage"
		// (max) read 80% for FY 2026, the year's utilisation, not July's 100%
		// (2026-11-08).
		if onTimeGrid && aggRule == "formula" && timeSummary != "sum" && timeSummary != "none" {
			out = append(out, fmt.Sprintf("%s totals by its formula (agg_rule \"formula\"), so its time_summary %q never applies: a year shows the formula on the year's totals, not the months' %s — "+
				"for the %s of the months set agg_rule \"none\" (its other totals then stay empty), or drop the time summary",
				name, timeSummary, timeSummary, timeSummary))
		}
		if onTimeGrid && timeSummary == "sum" && format != "percentage" && looksLikeLevel(name, label) {
			out = append(out, fmt.Sprintf("%s looks like a level (a headcount or a balance) and is summed over the months of its time grid: its FY is the months added up — set time_summary \"last\" (closing), \"first\" (opening) or \"average\"", name))
		}
	}
	return out
}

// looksLikeLevel: a name or label word naming a stock rather than a flow.
// A flow word beside it wins: new_restaurant_opening_capex is spent in its
// month, and "opening" there names an event, not an opening balance.
func looksLikeLevel(name, label string) bool {
	level := false
	for _, w := range nameWords(name + " " + label) {
		switch w {
		case "hc", "headcount", "fte", "ftes", "balance", "closing", "ending", "opening", "inventory", "stock":
			level = true
		case "capex", "cost", "costs", "expense", "expenses", "spend", "revenue", "sales", "fee", "fees",
			"payment", "payments", "investment", "change", "movement", "increase", "decrease", "depreciation", "interest":
			return false
		}
	}
	return level
}

// looksLikeCount: a metric whose name says it counts rows (a row count, a
// number of records) — where typed 1s should be a COUNTIFS. A 0/1 flag
// (seas_event: 1 in the months with an event) is entered, not counted.
func looksLikeCount(ref string) bool {
	for _, w := range nameWords(ref) {
		switch w {
		case "count", "counts", "rows", "row", "records", "entries", "lines", "number", "num", "n":
			return true
		}
	}
	return false
}

func nameWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '_' || r == ' ' || r == '-' || r == '.' || r == '(' || r == ')'
	})
}

// unorderedPagesProblems holds a plan that adds dashboards to an order: while
// a dashboard of the revision has no place in it, the tabs show those
// alphabetically after the ordered ones, and with none ordered everyone lands
// on whichever name sorts first ("Actuals & checks" before "Start here",
// 2026-11-01). The prompt asks for reorder_dashboards; this makes it so.
func unorderedPagesProblems(ctx context.Context, tx pgx.Tx, revID string, steps []aiassistant.ProposalStep) []string {
	adds := false
	for _, step := range steps {
		adds = adds || step.Tool == "create_dashboard"
	}
	if !adds {
		return nil
	}
	var total int
	var unordered []string
	var first string
	err := tx.QueryRow(ctx, `
		SELECT count(*),
		       COALESCE(array_agg(name ORDER BY name) FILTER (WHERE sort_order IS NULL), '{}'),
		       COALESCE((array_agg(name ORDER BY sort_order NULLS LAST, name))[1], '')
		FROM model.dashboard_def WHERE revision_id = $1::uuid`, revID).Scan(&total, &unordered, &first)
	if err != nil || total < 2 || len(unordered) == 0 {
		return nil
	}
	names := make([]string, len(unordered))
	for i, n := range unordered {
		names[i] = strconv.Quote(n)
	}
	return []string{fmt.Sprintf("dashboards %s have no place in the order: the tabs show them alphabetically after the "+
		"ordered ones, and everyone lands on %q. Add reorder_dashboards with every dashboard of the revision once: "+
		"the guide page (Start here) first, the pages in the order the work goes, the developer's page last",
		strings.Join(names, ", "), first)}
}

// offGridReadProblems names each formula the plan writes, on a metric on a
// grid, that reads a metric on no grid. A metric on no grid has no
// dimensions: read from a cell it is one value for the whole model, its
// total over every month, scenario and member. gross_profit left on no grid
// made ebitda and income tax read the model's total in every month
// (15,019,051 instead of 19,512; 2026-10-31). Nothing else refuses it.
func offGridReadProblems(ctx context.Context, tx pgx.Tx, modelID, revID string, steps []aiassistant.ProposalStep, created []string) []string {
	var refs []string
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
		refs = append(refs, strings.ToLower(p.Name), strings.ToLower(p.MetricID))
		if i < len(created) && created[i] != "" {
			refs = append(refs, created[i])
		}
	}
	if len(refs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT r.name, d.name
		FROM model.metric_def r
		JOIN model.calc_dependency cd ON cd.metric_id = r.id
		JOIN model.metric_def d ON d.id = cd.depends_on_metric_id AND d.id <> r.id
		WHERE r.model_id = $1::uuid AND r.revision_id = $2::uuid
		  AND (lower(r.name) = ANY($3) OR r.id::text = ANY($3))
		  AND EXISTS (SELECT 1 FROM model.grid_metric gm WHERE gm.metric_id = r.id)
		  AND NOT EXISTS (SELECT 1 FROM model.grid_metric gm WHERE gm.metric_id = d.id)
		ORDER BY r.name, d.name`, modelID, revID, refs)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var reader, offGrid string
		if rows.Scan(&reader, &offGrid) == nil {
			out = append(out, fmt.Sprintf("%s reads %s, which is on no grid: a metric on no grid has no dimensions, so every cell of %s would read its one total over the whole model — add %s to the grid %s is on (add_grid_metric) in this plan",
				reader, offGrid, reader, offGrid, reader))
		}
	}
	return out
}

// kpiTileWarnings names each KPI tile a plan adds or changes that adds up
// members nobody adds: pinned to one member (kpi_context_mode "pin") or
// showing the whole model ("total"), it totals every member of the
// metric's other dimensions — every scenario, every month of every year.
// gpt-5-mini pinned revenue tiles to the view dimension only, and they
// showed $141.9M for an FY2026 revenue of €20.1M (2026-10-20). A dimension
// the dashboard's selector bar holds, or a time dimension, left that way
// is named. created are the dry run's ids by step (add_dashboard_widget's
// widget).
func kpiTileWarnings(ctx context.Context, tx pgx.Tx, steps []aiassistant.ProposalStep, created []string) []string {
	var out []string
	seen := map[string]bool{}
	for i, step := range steps {
		var widgetID string
		switch step.Tool {
		case "add_dashboard_widget":
			if i < len(created) {
				widgetID = created[i]
			}
		case "update_dashboard_widget":
			var p struct {
				WidgetID string `json:"widget_id"`
			}
			_ = json.Unmarshal(step.Params, &p)
			widgetID = p.WidgetID
		default:
			continue
		}
		if widgetID == "" || seen[widgetID] {
			continue
		}
		seen[widgetID] = true
		var kind, title string
		var metricID *string
		var propsRaw, barRaw []byte
		if tx.QueryRow(ctx, `
			SELECT w.widget_type, w.ref_id, COALESCE(NULLIF(w.title,''), ''), COALESCE(w.widget_props, '{}'::jsonb), d.selector_bar
			FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id = w.dashboard_id
			WHERE w.id::text = $1`, widgetID).Scan(&kind, &metricID, &title, &propsRaw, &barRaw) != nil || kind != "metric_kpi" || metricID == nil {
			continue
		}
		var props struct {
			Mode  string `json:"kpi_context_mode"`
			Scope *struct {
				DimensionID string `json:"dimension_id"`
				MemberCode  string `json:"member_code"`
			} `json:"kpi_scope"`
		}
		_ = json.Unmarshal(propsRaw, &props)
		if props.Mode != "pin" && props.Mode != "total" {
			continue
		}
		var bar []struct {
			DimensionID string `json:"dimension_id"`
		}
		_ = json.Unmarshal(barRaw, &bar)
		inBar := map[string]bool{}
		for _, b := range bar {
			inBar[b.DimensionID] = true
		}
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT d.id::text, d.name, d.dimension_type = 'time'
			FROM model.grid_metric gm JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
			JOIN model.dimension_def d ON d.id = gd.dimension_id
			WHERE gm.metric_id::text = $1 ORDER BY d.name`, *metricID)
		if err != nil {
			continue
		}
		var summed []string
		for rows.Next() {
			var id, name string
			var isTime bool
			if rows.Scan(&id, &name, &isTime) != nil {
				continue
			}
			if props.Scope != nil && props.Scope.DimensionID == id {
				continue
			}
			if inBar[id] || isTime {
				summed = append(summed, name)
			}
		}
		rows.Close()
		if len(summed) == 0 {
			continue
		}
		label := title
		if label == "" {
			label = "KPI tile"
		}
		pinned := "shows the whole model"
		if props.Scope != nil {
			pinned = fmt.Sprintf("is pinned to %s only", props.Scope.MemberCode)
		}
		out = append(out, fmt.Sprintf(`step %d (%s %s): the tile %s, so it adds up every member of %s — every scenario, every period. `+
			`Set "kpi_context_mode": "bar" (it keeps kpi_scope and takes the rest from the selector bar), or pin those dimensions too`,
			i+1, step.Tool, label, pinned, strings.Join(summed, ", ")))
	}
	return out
}

// currencyWarnings names each metric a plan makes a currency amount
// without saying its currency: format_currency then stays "$", and the
// education model's euros showed as dollars ($141.9M, 2026-10-20). The
// symbol the model's other amounts use is suggested when there is one.
func currencyWarnings(ctx context.Context, tx pgx.Tx, modelID, revID string, steps []aiassistant.ProposalStep) []string {
	var usual string
	_ = tx.QueryRow(ctx, `
		SELECT format_currency FROM model.metric_def
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND format='currency' AND format_currency NOT IN ('', '$')
		GROUP BY 1 ORDER BY count(*) DESC LIMIT 1`, modelID, revID).Scan(&usual)
	var out []string
	for i, step := range steps {
		if step.Tool != "create_metric" && step.Tool != "update_metric" {
			continue
		}
		var p map[string]any
		if json.Unmarshal(step.Params, &p) != nil || p["format"] != "currency" {
			continue
		}
		if c, ok := p["format_currency"].(string); ok && strings.TrimSpace(c) != "" {
			continue
		}
		name, _ := p["name"].(string)
		if name == "" {
			name, _ = p["metric_id"].(string)
		}
		hint := `say which: "format_currency": "€" (or "£", "$", "CHF" …) — left out, the amounts show as dollars`
		if usual != "" {
			hint = fmt.Sprintf(`the model's other amounts are in %s: set "format_currency": %q — left out, these show as dollars`, usual, usual)
		}
		out = append(out, fmt.Sprintf("step %d (%s %s): a currency amount without format_currency: %s", i+1, step.Tool, name, hint))
	}
	return out
}

// dashboardLayoutProblems holds every dashboard a plan touches to a layout
// people can read, as the console draws it: rows from the top (widgets
// within 20px of a row's top share it), side by side on 20px columns. The
// AI build's management dashboard (2026-11-01) had a fourth tile dropped to
// a row of its own, a chart half-way down a row drawn under it, a one-row
// grid in a 600px box, a nine-row grid in 170px, KPI tiles following a
// selector bar the page did not have, and a text line "Selectors:
// Scenario=Base" with each widget choosing its own scenario.
func dashboardLayoutProblems(ctx context.Context, tx pgx.Tx, revID string, steps []aiassistant.ProposalStep, created []string) []string {
	var problems []string
	for id := range touchedDashboards(ctx, tx, revID, steps, created) {
		p, _ := oneDashboardLayoutProblems(ctx, tx, id)
		problems = append(problems, p...)
	}
	sort.Strings(problems)
	return problems
}

// dashboardLayoutWarnings: the warnings of the dashboards a plan touches.
func dashboardLayoutWarnings(ctx context.Context, tx pgx.Tx, revID string, steps []aiassistant.ProposalStep, created []string) []string {
	var warnings []string
	for id := range touchedDashboards(ctx, tx, revID, steps, created) {
		_, w := oneDashboardLayoutProblems(ctx, tx, id)
		warnings = append(warnings, w...)
	}
	sort.Strings(warnings)
	return warnings
}

// otherDashboardsWarnings names, once, the dashboards a plan touching
// dashboards leaves with layout problems ("fix every page" fixed the four
// with a "Selectors:" line and left three cramped, 2026-11-01).
func otherDashboardsWarnings(ctx context.Context, tx pgx.Tx, revID string, steps []aiassistant.ProposalStep, created []string) []string {
	touched := touchedDashboards(ctx, tx, revID, steps, created)
	if len(touched) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM model.dashboard_def WHERE revision_id = $1::uuid ORDER BY `+modeledit.DashboardOrderSQL(""), revID)
	if err != nil {
		return nil
	}
	var others []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && !touched[id] {
			others = append(others, id)
		}
	}
	rows.Close()
	var out []string
	for _, id := range others {
		if p, _ := oneDashboardLayoutProblems(ctx, tx, id); len(p) > 0 {
			out = append(out, p[0]+fmt.Sprintf(" (and %d more on that page) — the plan leaves this page as it is", len(p)-1))
		}
	}
	return out
}

// touchedDashboards: the dashboards a plan creates or changes, by id.
func touchedDashboards(ctx context.Context, tx pgx.Tx, revID string, steps []aiassistant.ProposalStep, created []string) map[string]bool {
	touched := map[string]bool{}
	find := func(ref string) {
		var id string
		if tx.QueryRow(ctx, `SELECT id::text FROM model.dashboard_def WHERE revision_id = $1::uuid
			AND (id::text = $2 OR lower(name) = lower($2)) LIMIT 1`, revID, strings.TrimSpace(ref)).Scan(&id) == nil {
			touched[id] = true
		}
	}
	for i, step := range steps {
		var p struct {
			DashboardID string `json:"dashboard_id"`
			WidgetID    string `json:"widget_id"`
		}
		_ = json.Unmarshal(step.Params, &p)
		switch step.Tool {
		case "create_dashboard":
			if i < len(created) && created[i] != "" {
				find(created[i])
			}
		case "add_dashboard_widget", "update_dashboard", "arrange_dashboard":
			find(p.DashboardID)
		case "update_dashboard_widget":
			var id string
			if tx.QueryRow(ctx, `SELECT dashboard_id::text FROM model.dashboard_widget WHERE id::text = $1`, p.WidgetID).Scan(&id) == nil {
				touched[id] = true
			}
		}
	}
	return touched
}

type layoutWidget struct {
	id, typ, title, ref, content, label string
	props                               json.RawMessage
	x, y, w, h                          int
	shown                               bool
}

func (w layoutWidget) name() string {
	if w.title != "" {
		return fmt.Sprintf("%s %q", w.typ, w.title)
	}
	return fmt.Sprintf("%s %s", w.typ, w.id)
}

// oneDashboardLayoutProblems: what a plan must fix on one dashboard, and what
// goes back to the model once as a warning (titles and tiles that mislead
// but do not break the page: refused together across ten pages, they left
// gpt-5-mini without a plan after its retries, 2026-11-01).
func oneDashboardLayoutProblems(ctx context.Context, tx pgx.Tx, dashID string) (problems, warnings []string) {
	var dashName string
	var barRaw json.RawMessage
	if tx.QueryRow(ctx, `SELECT name, COALESCE(selector_bar, '[]'::jsonb) FROM model.dashboard_def WHERE id = $1::uuid`, dashID).Scan(&dashName, &barRaw) != nil {
		return nil, nil
	}
	var bar []struct {
		DimensionID string `json:"dimension_id"`
	}
	_ = json.Unmarshal(barRaw, &bar)
	onBar := map[string]bool{}
	for _, e := range bar {
		onBar[e.DimensionID] = true
	}
	rows, err := tx.Query(ctx, `SELECT w.id::text, w.widget_type, COALESCE(w.title, ''), COALESCE(w.ref_id::text, ''), COALESCE(w.content, ''),
		COALESCE(w.widget_props, '{}'::jsonb), COALESCE(w.pos_x, 0), COALESCE(w.pos_y, 0), COALESCE(w.size_w, 300), COALESCE(w.size_h, 0),
		COALESCE(w.show_title, false), COALESCE(NULLIF(m.label, ''), m.name, '')
		FROM model.dashboard_widget w LEFT JOIN model.metric_def m ON w.widget_type = 'metric_kpi' AND m.id::text = w.ref_id::text
		WHERE w.dashboard_id = $1::uuid ORDER BY w.pos_y, w.pos_x`, dashID)
	if err != nil {
		return nil, nil
	}
	var ws []layoutWidget
	for rows.Next() {
		var w layoutWidget
		if rows.Scan(&w.id, &w.typ, &w.title, &w.ref, &w.content, &w.props, &w.x, &w.y, &w.w, &w.h, &w.shown, &w.label) == nil {
			ws = append(ws, w)
		}
	}
	rows.Close()
	var out, warns []string
	say := func(format string, args ...any) {
		out = append(out, fmt.Sprintf("dashboard %q: ", dashName)+fmt.Sprintf(format, args...))
	}
	warn := func(format string, args ...any) {
		warns = append(warns, fmt.Sprintf("dashboard %q: ", dashName)+fmt.Sprintf(format, args...))
	}
	const arrange = " — arrange_dashboard {\"dashboard_id\", \"rows\": [[widget ids, left to right], …]} places and sizes every widget"

	// Rows as the console groups them.
	type row struct {
		top, bottom int
		items       []layoutWidget
	}
	var grouped []*row
	for _, w := range ws {
		var r *row
		for _, g := range grouped {
			if abs(g.top-w.y) <= 20 {
				r = g
				break
			}
		}
		if r == nil {
			for _, g := range grouped {
				if w.y > g.top+20 && w.y < g.bottom {
					say("%s starts at y=%d, inside the row at y=%d (to y=%d): rows stack, so it is drawn below that whole row"+arrange, w.name(), w.y, g.top, g.bottom)
					break
				}
			}
			r = &row{top: w.y}
			grouped = append(grouped, r)
		}
		r.items = append(r.items, w)
		r.bottom = max(r.bottom, w.y+max(w.h, 40))
	}
	col := func(px int) int { return int(math.Round(float64(px) / 20)) }
	for _, g := range grouped {
		for i := 0; i < len(g.items); i++ {
			a := g.items[i]
			if a.x+a.w > 1200 {
				say("%s reaches x=%d, past the 1200px canvas: the page scrolls sideways"+arrange, a.name(), a.x+a.w)
			}
			for j := i + 1; j < len(g.items); j++ {
				b := g.items[j]
				if col(a.x) < col(b.x+b.w) && col(b.x) < col(a.x+a.w) {
					say("%s and %s overlap in the row at y=%d: one is pushed to a row of its own"+arrange, a.name(), b.name(), g.top)
				}
			}
		}
	}

	// Sizes against content.
	for _, w := range ws {
		if w.typ != "grid" {
			continue
		}
		need := modeledit.WidgetHeight(ctx, tx, w.typ, w.ref, w.props, w.content)
		switch {
		case w.h > 0 && w.h+80 < min(need, 400):
			say("%s is %dpx tall for rows that need about %dpx: they scroll inside a cramped box"+arrange, w.name(), w.h, need)
		case w.h > need+160:
			say("%s is %dpx tall for rows that need about %dpx: the rest of the box is empty"+arrange, w.name(), w.h, need)
		}
	}

	// Selectors: tiles following a bar the page lacks; a dimension each
	// widget picks on its own.
	for _, w := range ws {
		var p struct {
			Mode string `json:"kpi_context_mode"`
		}
		_ = json.Unmarshal(w.props, &p)
		if w.typ == "metric_kpi" && p.Mode == "bar" && len(bar) == 0 {
			say("%s follows the selector bar, but the dashboard has none, so it adds up every scenario and period — give the dashboard a selector_bar (update_dashboard)", w.name())
		}
		var sc struct {
			Scope *struct {
				DimensionID string `json:"dimension_id"`
				Member      string `json:"member_code"`
			} `json:"kpi_scope"`
		}
		_ = json.Unmarshal(w.props, &sc)
		if w.typ == "metric_kpi" && (p.Mode == "bar" || p.Mode == "") && sc.Scope != nil && onBar[sc.Scope.DimensionID] {
			warn("%s pins %s on a dimension the selector bar sets: the bar wins, so the tile repeats the bar's figure — compare with \"kpi_compare\" {dimension_id, member_code} on the tile it repeats, or remove it", w.name(), sc.Scope.Member)
		}
		if t := strings.TrimSpace(w.content); w.typ == "text" && len(bar) > 0 && !strings.Contains(t, "\n") &&
			strings.HasPrefix(strings.ToLower(strings.TrimLeft(t, "#*_ ")), "selector") {
			say("the text %q stands in for selectors the bar now shows: delete it (delete_dashboard_widget %s)", t, w.id)
		}
		if (w.typ == "chart" || w.typ == "grid") && (strings.TrimSpace(w.title) == "" || !w.shown) {
			say("the %s %s shows no title: give it one saying what it shows, with the unit (\"title\" and \"show_title\": true on update_dashboard_widget)", w.typ, w.id)
		}
	}
	// The same tile twice (renamed "(secondary)" to read differently), a
	// tile adding up every month of every year (the bar without the period:
	// €33.3M for two years), a title that only repeats its grid's name.
	sameTile := map[string][]string{}
	for _, w := range ws {
		switch w.typ {
		case "metric_kpi":
			var k struct {
				Mode    string          `json:"kpi_context_mode"`
				Scope   json.RawMessage `json:"kpi_scope"`
				Compare json.RawMessage `json:"kpi_compare"`
			}
			_ = json.Unmarshal(w.props, &k)
			key := w.ref + "|" + k.Mode + "|" + string(k.Scope) + "|" + string(k.Compare)
			sameTile[key] = append(sameTile[key], w.name())
			if k.Mode != "bar" && k.Mode != "" {
				continue
			}
			var scopeDim string
			var sc struct {
				Scope *struct {
					DimensionID string `json:"dimension_id"`
				} `json:"kpi_scope"`
			}
			if json.Unmarshal(w.props, &sc) == nil && sc.Scope != nil {
				scopeDim = sc.Scope.DimensionID
			}
			trows, err := tx.Query(ctx, `SELECT DISTINCT d.id::text, d.name FROM model.grid_metric gm
				JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
				JOIN model.dimension_def d ON d.id = gd.dimension_id
				WHERE gm.metric_id::text = $1 AND d.dimension_type = 'time'`, w.ref)
			if err != nil {
				continue
			}
			for trows.Next() {
				var did, dname string
				if trows.Scan(&did, &dname) == nil && !onBar[did] && did != scopeDim && len(bar) > 0 {
					warn("%s adds up every %s of every year: put %s on the selector bar (opening on the current year) or pin a period", w.name(), dname, dname)
				}
			}
			trows.Close()
		case "chart", "grid":
			var gridName string
			if w.title != "" && tx.QueryRow(ctx, `SELECT name FROM model.grid_def WHERE id::text = $1`, w.ref).Scan(&gridName) == nil {
				bare := func(t string) string {
					t = strings.ToLower(strings.TrimSpace(t))
					if i := strings.IndexAny(t, ",("); i > 0 {
						t = strings.TrimSpace(t[:i])
					}
					return t
				}
				if bare(w.title) == bare(gridName) && w.typ == "chart" {
					warn("the chart %q is titled by its grid's name: say what it shows — the metric and the unit (\"EBITDA by month, EUR\")", w.title)
				}
			}
		}
	}
	for _, names := range sameTile {
		if len(names) > 1 {
			warn("%s show the same figure: keep one; a comparison is \"kpi_compare\" on that tile, not a second tile", strings.Join(names, ", "))
		}
	}

	// Tiles that read alike: the same metric twice (actual and its variance)
	// both said "NET REVENUE".
	reads := map[string][]string{}
	for _, w := range ws {
		if w.typ != "metric_kpi" {
			continue
		}
		label := w.label
		if w.shown && strings.TrimSpace(w.title) != "" {
			label = w.title
		}
		reads[strings.ToLower(strings.TrimSpace(label))] = append(reads[strings.ToLower(strings.TrimSpace(label))], w.id)
	}
	for label, ids := range reads {
		if label != "" && len(ids) > 1 {
			warn("%d KPI tiles read %q (%s): title each by what it shows (\"Net revenue vs budget, EUR\") with \"title\" and \"show_title\": true", len(ids), label, strings.Join(ids, ", "))
		}
	}
	pickers := map[string][]string{}
	for _, w := range ws {
		if (w.typ != "grid" && w.typ != "chart") || w.ref == "" {
			continue
		}
		axes := map[string]bool{}
		var p struct {
			DefaultView *struct {
				Rows []string `json:"rows"`
				Cols []string `json:"cols"`
			} `json:"default_view"`
			Chart struct {
				DimensionID string `json:"dimension_id"`
				SeriesDim   string `json:"series_dimension_id"`
			} `json:"chart"`
		}
		_ = json.Unmarshal(w.props, &p)
		if p.DefaultView != nil {
			for _, d := range append(p.DefaultView.Rows, p.DefaultView.Cols...) {
				axes[d] = true
			}
		}
		axes[p.Chart.DimensionID], axes[p.Chart.SeriesDim] = true, true
		drows, err := tx.Query(ctx, `SELECT gd.dimension_id::text, d.name FROM model.grid_dimension gd
			JOIN model.dimension_def d ON d.id = gd.dimension_id
			WHERE gd.grid_id = $1::uuid AND (SELECT count(*) FROM model.dimension_member m WHERE m.dimension_id = gd.dimension_id) > 1`, w.ref)
		if err != nil {
			continue
		}
		for drows.Next() {
			var did, dname string
			if drows.Scan(&did, &dname) == nil && !axes[did] && !onBar[did] {
				pickers[dname] = append(pickers[dname], w.name())
			}
		}
		drows.Close()
	}
	for dim, who := range pickers {
		if len(who) >= 2 {
			say("%d widgets each draw their own %s selector (%s): put %s on the dashboard's selector_bar (update_dashboard) so one selector drives them all", len(who), dim, strings.Join(who, ", "), dim)
		}
	}
	return out, warns
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// emptyInputWarnings names the revision's input metrics that hold no value
// and that nothing fills — no table posts into them, no saved loader maps a
// column to them — when a plan builds pages (2026-11-06: a build of 251
// metrics had every input grid at 0 values while the assistant called the
// sheets done, and every calculation read zeros). Only at that stage: a plan
// creating inputs before their values load is the staging the instructions
// ask for, and a warning sends the plan back to the model once.
func emptyInputWarnings(ctx context.Context, tx pgx.Tx, modelID, revID string, steps []aiassistant.ProposalStep) []string {
	pages := false
	for _, step := range steps {
		switch step.Tool {
		case "create_dashboard", "add_dashboard_widget", "update_dashboard_widget", "arrange_dashboard":
			pages = true
		}
	}
	if !pages {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT md.name FROM model.metric_def md
		WHERE md.model_id = $1::uuid AND md.revision_id = $2::uuid AND md.is_input
		  AND COALESCE(md.format, '') <> 'text'
		  AND EXISTS (SELECT 1 FROM model.grid_metric gm WHERE gm.metric_id = md.id)
		  AND NOT EXISTS (SELECT 1 FROM runtime.fact_input f WHERE f.metric_id = md.id)
		  AND NOT EXISTS (SELECT 1 FROM model.form_metric_mapping fm WHERE fm.target_metric_id = md.id)
		  AND NOT EXISTS (SELECT 1 FROM model.integration_def i, jsonb_each_text(COALESCE(i.config->'column_map', '{}'::jsonb)) c
		                  WHERE i.model_id = md.model_id AND (i.revision_id = md.revision_id OR i.revision_id IS NULL)
		                    AND lower(c.value) = lower(md.name))
		ORDER BY md.name`, modelID, revID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var empty []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			empty = append(empty, n)
		}
	}
	if len(empty) == 0 {
		return nil
	}
	shown := empty
	if len(shown) > 12 {
		shown = shown[:12]
	}
	more := ""
	if len(empty) > len(shown) {
		more = fmt.Sprintf(" and %d more", len(empty)-len(shown))
	}
	return []string{fmt.Sprintf("this plan builds pages, but %d input metric(s) hold no value and nothing loads them (%s%s): every calculation reading them reads 0. "+
		"Load them from the attached sheet (import_file_data) or a saved loader, and check a row with compare_with_sheet",
		len(empty), strings.Join(shown, ", "), more)}
}

// memberlessDimensionWarnings names each dimension a plan places on a grid
// (create_grid's dimensions, add_grid_dimension) that has no member as the
// plan leaves it: the grid shows no rows along it until members are added,
// and nothing said so (2026-11-08). A dimension a table keeps is left out —
// its members are the records that table gets.
func memberlessDimensionWarnings(ctx context.Context, tx pgx.Tx, revID string, steps []aiassistant.ProposalStep) []string {
	var out []string
	seen := map[string]bool{}
	for i, step := range steps {
		var p struct {
			GridID      string   `json:"grid_id"`
			Name        string   `json:"name"`
			Dimensions  []string `json:"dimensions"`
			DimensionID string   `json:"dimension_id"`
		}
		if json.Unmarshal(step.Params, &p) != nil {
			continue
		}
		var dims []string
		grid := p.Name
		switch step.Tool {
		case "create_grid":
			dims = p.Dimensions
		case "add_grid_dimension":
			dims, grid = []string{p.DimensionID}, p.GridID
		default:
			continue
		}
		for _, ref := range dims {
			ref = strings.TrimSpace(ref)
			if ref == "" || seen[strings.ToLower(ref)] {
				continue
			}
			var name string
			var members int
			var kept bool
			if tx.QueryRow(ctx, `
				SELECT d.name, (SELECT count(*) FROM model.dimension_member m WHERE m.dimension_id = d.id),
				       EXISTS (SELECT 1 FROM model.form_def f WHERE f.keeps_dimension_id = d.id)
				FROM model.dimension_def d
				WHERE d.revision_id = $1::uuid AND (d.id::text = $2 OR lower(d.name) = lower($2))`,
				revID, ref).Scan(&name, &members, &kept) != nil || members > 0 || kept {
				continue
			}
			seen[strings.ToLower(ref)] = true
			out = append(out, fmt.Sprintf("step %d (%s %s): dimension %s has no members as this plan leaves it — add its members first (in this plan, or by importing them): the grid shows no rows along it until it has some",
				i+1, step.Tool, grid, name))
		}
	}
	return out
}
