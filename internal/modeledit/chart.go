package modeledit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// CheckChartMetrics refuses a chart widget whose series are not on the grid
// it reads. A chart is drawn from its grid alone: a metric of another grid
// saved cleanly and every chart-data read then answered 403 ("metric … not
// accessible or not in grid") — found only when the dashboard rendered.
// The plotted dimension must be one of the grid's too, and a chart must
// name its grid. Props without a chart are not this check's business.
func CheckChartMetrics(ctx context.Context, db DB, gridID string, widgetProps json.RawMessage) error {
	if len(widgetProps) == 0 || string(widgetProps) == "null" {
		return nil
	}
	var props struct {
		Chart *struct {
			DimensionID string   `json:"dimension_id"`
			MetricIDs   []string `json:"metric_ids"`
			XMetricID   string   `json:"x_metric_id"`
			YMetricID   string   `json:"y_metric_id"`
		} `json:"chart"`
	}
	if json.Unmarshal(widgetProps, &props) != nil || props.Chart == nil {
		return nil
	}
	c := props.Chart
	if gridID == "" {
		// A chart is drawn from one grid; one saved without it never draws.
		return fmt.Errorf("a chart reads one grid: set ref_id to the grid that holds its metrics%s", seriesGrids(ctx, db, c.MetricIDs))
	}
	var gridName string
	if err := db.QueryRow(ctx, `SELECT name FROM model.grid_def WHERE id=$1::uuid`, gridID).Scan(&gridName); err != nil {
		return nil // not a grid id: the widget's own ref check speaks to that
	}
	onGrid := map[string]bool{}
	rows, err := db.Query(ctx, `SELECT metric_id::text FROM model.grid_metric WHERE grid_id=$1::uuid`, gridID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		onGrid[id] = true
	}
	rows.Close()
	var missing []string
	for _, id := range append(append([]string{}, c.MetricIDs...), c.XMetricID, c.YMetricID) {
		if id == "" || onGrid[id] {
			continue
		}
		name := id
		_ = db.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, id).Scan(&name)
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		return fmt.Errorf("the chart reads grid %q, which does not hold %s: a chart plots only its own grid's metrics. "+
			"Point it at the grid that holds them, or give that grid a metric reading each one (formula = the other metric) and plot those%s",
			gridName, strings.Join(missing, ", "), seriesGrids(ctx, db, c.MetricIDs))
	}
	if c.DimensionID != "" {
		var ok bool
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.grid_dimension WHERE grid_id=$1::uuid AND dimension_id=$2::uuid)`,
			gridID, c.DimensionID).Scan(&ok); err == nil && !ok {
			return fmt.Errorf("the chart plots along a dimension grid %q does not have", gridName)
		}
	}
	return nil
}

// seriesGrids says where a chart's series live: the one grid to point it
// at, or — when they sit on different grids — which grid holds which and how
// to bring them onto one.
func seriesGrids(ctx context.Context, db DB, metricIDs []string) string {
	byGrid := map[string][]string{}
	var order []string
	for _, id := range metricIDs {
		var metric, grid string
		if err := db.QueryRow(ctx, `SELECT m.name, COALESCE(g.name, '') FROM model.metric_def m
			LEFT JOIN model.grid_metric gm ON gm.metric_id = m.id LEFT JOIN model.grid_def g ON g.id = gm.grid_id
			WHERE m.id = $1::uuid`, id).Scan(&metric, &grid); err != nil {
			continue
		}
		if grid == "" {
			grid = "no grid"
		}
		if _, ok := byGrid[grid]; !ok {
			order = append(order, grid)
		}
		byGrid[grid] = append(byGrid[grid], metric)
	}
	switch len(order) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(" (%q holds them)", order[0])
	}
	parts := make([]string, 0, len(order))
	for _, g := range order {
		parts = append(parts, fmt.Sprintf("%s on %q", strings.Join(byGrid[g], ", "), g))
	}
	return fmt.Sprintf(". The series are on different grids (%s): make a grid whose only dimension is the plotted one (a grid with more dimensions would repeat each copy per member), put on it a metric for each series that reads it (formula = that metric), and chart that grid",
		strings.Join(parts, "; "))
}
