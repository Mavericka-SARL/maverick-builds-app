package writeguard

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// Cell is one value being written: an input metric at one member of each of
// its dimensions (dimension id → member code).
type Cell struct {
	MetricID string
	DimCodes map[string]string
}

// ClosedReason reports why the first of cells takes no input — a cell where
// one of its metric's closed rules holds (metricformula.ClosedRule) — or ""
// when every cell is open. A rule reads its calculated metric's stored value
// at the same cell (runtime.calc_result, the cell's members on that metric's
// dimensions); a blank counts as 0. So a rule follows a setting such as
// Actual Through Month once recalculation has stored the new values.
//
// Every write entry point that writes typed values calls it next to
// CheckWriteMetrics: the grid's cells and batches, the AI Developer's
// write_input_values, file imports.
func ClosedReason(ctx context.Context, pool *pgxpool.Pool, revisionID string, cells []Cell) (string, error) {
	if len(cells) == 0 {
		return "", nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, c := range cells {
		if c.MetricID != "" && !seen[c.MetricID] {
			seen[c.MetricID] = true
			ids = append(ids, c.MetricID)
		}
	}
	rows, err := pool.Query(ctx, `SELECT id::text, COALESCE(NULLIF(label,''), name), closed_when FROM model.metric_def
		WHERE id::text = ANY($1) AND closed_when <> '[]'::jsonb`, ids)
	if err != nil {
		return "", fmt.Errorf("load closed rules: %w", err)
	}
	type closed struct {
		label string
		rules []metricformula.ClosedRule
	}
	byMetric := map[string]closed{}
	for rows.Next() {
		var id, label string
		var raw []byte
		if err := rows.Scan(&id, &label, &raw); err != nil {
			rows.Close()
			return "", err
		}
		var rules []metricformula.ClosedRule
		if json.Unmarshal(raw, &rules) == nil && len(rules) > 0 {
			byMetric[id] = closed{label: label, rules: rules}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(byMetric) == 0 {
		return "", nil
	}

	// The values of each rule's metric, keyed by its cell's members.
	type condition struct {
		dims   []string
		values map[string]float64
	}
	conditions := map[string]*condition{}
	load := func(name string) (*condition, error) {
		if c, ok := conditions[strings.ToLower(name)]; ok {
			return c, nil
		}
		var condID string
		if err := pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE revision_id=$1::uuid AND lower(name)=lower($2) LIMIT 1`,
			revisionID, name).Scan(&condID); err != nil {
			return nil, fmt.Errorf("closed rule names %q, which is no metric of this revision", name)
		}
		c := &condition{values: map[string]float64{}}
		drows, err := pool.Query(ctx, `SELECT DISTINCT gd.dimension_id::text FROM model.grid_metric gm
			JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id WHERE gm.metric_id = $1::uuid`, condID)
		if err != nil {
			return nil, err
		}
		for drows.Next() {
			var d string
			if err := drows.Scan(&d); err != nil {
				drows.Close()
				return nil, err
			}
			c.dims = append(c.dims, d)
		}
		drows.Close()
		sort.Strings(c.dims)
		vrows, err := pool.Query(ctx, `SELECT DISTINCT ON (dim_members) dim_members, COALESCE(value, 0)
			FROM runtime.calc_result WHERE revision_id=$1::uuid AND metric_id=$2::uuid
			ORDER BY dim_members, calc_at DESC`, revisionID, condID)
		if err != nil {
			return nil, err
		}
		for vrows.Next() {
			var raw []byte
			var v float64
			if err := vrows.Scan(&raw, &v); err != nil {
				vrows.Close()
				return nil, err
			}
			var codes map[string]string
			if json.Unmarshal(raw, &codes) == nil {
				c.values[cellKey(c.dims, codes)] = v
			}
		}
		vrows.Close()
		if err := vrows.Err(); err != nil {
			return nil, err
		}
		conditions[strings.ToLower(name)] = c
		return c, nil
	}

	for _, cell := range cells {
		m, ok := byMetric[cell.MetricID]
		if !ok {
			continue
		}
		for _, r := range m.rules {
			c, err := load(r.Metric)
			if err != nil {
				return "", err
			}
			if r.Holds(c.values[cellKey(c.dims, cell.DimCodes)]) {
				return fmt.Sprintf("%s takes no input here: this cell is closed (%s %s %s)%s",
					m.label, r.Metric, r.Op, formatRuleValue(r.Value), cellWhere(cell.DimCodes)), nil
			}
		}
	}
	return "", nil
}

// cellKey is the cell's members on dims, in dims' order.
func cellKey(dims []string, codes map[string]string) string {
	parts := make([]string, len(dims))
	for i, d := range dims {
		parts[i] = d + "=" + codes[d]
	}
	return strings.Join(parts, "|")
}

func cellWhere(codes map[string]string) string {
	if len(codes) == 0 {
		return ""
	}
	vals := make([]string, 0, len(codes))
	for _, v := range codes {
		vals = append(vals, v)
	}
	sort.Strings(vals)
	return " at " + strings.Join(vals, " / ")
}

func formatRuleValue(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}
