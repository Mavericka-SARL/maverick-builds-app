package gateway

import (
	"context"
	"encoding/json"
	"fmt"
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
