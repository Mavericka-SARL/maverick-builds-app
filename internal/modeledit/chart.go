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
// The plotted dimension must be one of the grid's too. Props without a
// chart, and a chart on no grid, are not this check's business.
func CheckChartMetrics(ctx context.Context, db DB, gridID string, widgetProps json.RawMessage) error {
	if gridID == "" || len(widgetProps) == 0 || string(widgetProps) == "null" {
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
			"Point it at the grid that holds them, or give that grid a metric reading each one (formula = the other metric) and plot those",
			gridName, strings.Join(missing, ", "))
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
