package modeledit

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CheckWidgetProps refuses widget props naming metrics or dimensions the
// widget's grid does not hold: a chart's series and plotted dimension
// (CheckChartMetrics), a grid widget's chosen metrics (CheckGridWidgetMetrics).
func CheckWidgetProps(ctx context.Context, db DB, widgetType, gridID string, widgetProps json.RawMessage) error {
	switch widgetType {
	case "chart":
		return CheckChartMetrics(ctx, db, gridID, widgetProps)
	case "grid":
		return CheckGridWidgetMetrics(ctx, db, gridID, widgetProps)
	}
	return nil
}

// CheckGridWidgetMetrics refuses a grid widget whose metric_ids — the
// metrics it shows, in that order; absent or empty, all of the grid's — name
// a metric its grid does not hold, or one twice. A grid widget draws only its
// grid's metrics, so any other id would just be left out of the table.
func CheckGridWidgetMetrics(ctx context.Context, db DB, gridID string, widgetProps json.RawMessage) error {
	if len(widgetProps) == 0 || string(widgetProps) == "null" {
		return nil
	}
	var props map[string]json.RawMessage
	if json.Unmarshal(widgetProps, &props) != nil {
		return nil
	}
	raw, ok := props["metric_ids"]
	if !ok || string(raw) == "null" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return fmt.Errorf("metric_ids is the list of metric ids the grid widget shows, in order: %w", err)
	}
	if len(ids) == 0 || gridID == "" {
		return nil
	}
	var gridName string
	if err := db.QueryRow(ctx, `SELECT name FROM model.grid_def WHERE id=$1::uuid`, gridID).Scan(&gridName); err != nil {
		return nil // not a grid id: the widget's own ref check speaks to that
	}
	onGrid, err := gridMetricSet(ctx, db, gridID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var missing []string
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("metric_ids names %s twice", metricName(ctx, db, id))
		}
		seen[id] = true
		if !onGrid[id] {
			missing = append(missing, metricName(ctx, db, id))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the grid widget shows grid %q, which does not hold %s: a grid widget shows only its own grid's metrics — "+
			"add them to that grid first, or point the widget at the grid that holds them", gridName, strings.Join(missing, ", "))
	}
	return nil
}

// gridMetricSet is the set of the grid's metric ids.
func gridMetricSet(ctx context.Context, db DB, gridID string) (map[string]bool, error) {
	onGrid := map[string]bool{}
	rows, err := db.Query(ctx, `SELECT metric_id::text FROM model.grid_metric WHERE grid_id=$1::uuid`, gridID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		onGrid[id] = true
	}
	return onGrid, rows.Err()
}

// metricName names a metric id for a message; an unknown id stays as given.
func metricName(ctx context.Context, db DB, id string) string {
	name := id
	_ = db.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, id).Scan(&name)
	return name
}

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
	onGrid, err := gridMetricSet(ctx, db, gridID)
	if err != nil {
		return err
	}
	var missing []string
	for _, id := range append(append([]string{}, c.MetricIDs...), c.XMetricID, c.YMetricID) {
		if id == "" || onGrid[id] {
			continue
		}
		missing = append(missing, metricName(ctx, db, id))
	}
	var picklists []string
	for _, id := range append(append([]string{}, c.MetricIDs...), c.XMetricID, c.YMetricID) {
		if id == "" {
			continue
		}
		var isPicklist bool
		_ = db.QueryRow(ctx, `SELECT picklist_dimension_id IS NOT NULL FROM model.metric_def WHERE id=$1::uuid`, id).Scan(&isPicklist)
		if isPicklist {
			picklists = append(picklists, metricName(ctx, db, id))
		}
	}
	if len(picklists) > 0 {
		return fmt.Errorf("%s holds dimension members (a pick-list), not quantities, so a chart cannot plot it — "+
			"chart a number that counts or sums by it instead (COUNTIFS / SUMIFS over the pick-list)", strings.Join(picklists, ", "))
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

// WidgetTypes are the dashboard widget types the console renders
// (DashboardWidgets.tsx). Another type saved, drew nothing, and said
// nothing: the AI Developer added "workflow_button" widgets for a trigger.
var WidgetTypes = []string{"grid", "chart", "form", "metric_kpi", "automation_button", "integration_button", "text", "image", "import"}

// CheckWidgetType refuses a widget type the console does not render.
func CheckWidgetType(t string) error {
	for _, w := range WidgetTypes {
		if t == w {
			return nil
		}
	}
	hint := ""
	if strings.Contains(t, "button") || strings.Contains(t, "workflow") || strings.Contains(t, "trigger") {
		hint = ` — a button that starts a workflow is "automation_button" over a manual automation rule (ref_id: the rule)`
	}
	return fmt.Errorf("there is no widget type %q: the types are %s%s", t, strings.Join(WidgetTypes, ", "), hint)
}

// WidgetPropKeys are the widget_props keys the console reads (WidgetProps in
// web/src/api/client.ts), and ChartSettingKeys those of widget_props.chart.
// A key outside them was saved and never read: live, a KPI tile scoped
// {"Scenario": "RF", "Month": "FY2026"} showed the whole model's total, and
// a button's "button_label" left it saying "Trigger".
var (
	WidgetPropKeys = map[string]bool{"selectors_position": true, "background": true, "font_size": true, "font_weight": true,
		"color": true, "font_family": true, "alt": true, "image_fit": true, "button_color": true, "default_view": true,
		"metric_ids": true, "chart": true, "context": true, "kpi_scope": true, "kpi_context_mode": true, "confirm_text": true,
		"sync_context": true, "show_members": true, "rows_collapsed": true}
	ChartSettingKeys = map[string]bool{"chart_type": true, "dimension_id": true, "metric_ids": true, "x_metric_id": true,
		"y_metric_id": true, "context_defaults": true, "bin_count": true, "show_legend": true, "show_values": true,
		"value_format": true, "refresh_seconds": true, "hide_rollup_members": true}
)

// CheckWidgetPropKeys refuses a widget_props key — at the top or under
// "chart" — the console does not read. A key already in stored (a widget
// saved before this check) passes, so an old widget stays editable.
func CheckWidgetPropKeys(props, stored json.RawMessage) error {
	if len(props) == 0 || string(props) == "null" {
		return nil
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(props, &p); err != nil {
		return fmt.Errorf("widget_props is not a JSON object: %w", err)
	}
	var s map[string]json.RawMessage
	_ = json.Unmarshal(stored, &s)
	for k := range p {
		if _, old := s[k]; !WidgetPropKeys[k] && !old {
			return fmt.Errorf("widget_props has no key %q: its keys are %s", k, sortedKeys(WidgetPropKeys))
		}
	}
	if raw, ok := p["chart"]; ok && string(raw) != "null" {
		var chart, storedChart map[string]json.RawMessage
		if err := json.Unmarshal(raw, &chart); err != nil {
			return fmt.Errorf("widget_props.chart is not a JSON object: %w", err)
		}
		_ = json.Unmarshal(s["chart"], &storedChart)
		for k := range chart {
			if _, old := storedChart[k]; !ChartSettingKeys[k] && !old {
				return fmt.Errorf("a chart's settings have no key %q: they are %s", k, sortedKeys(ChartSettingKeys))
			}
		}
	}
	return nil
}

// StripUnreadWidgetProps drops the widget_props keys — at the top or under
// "chart" — the console does not read, and names them ("chart.<key>" for a
// chart setting). The developer API refuses such a key (CheckWidgetPropKeys);
// a model package, older or edited by hand, may still carry one. Props that
// are not a JSON object are returned as they are.
func StripUnreadWidgetProps(props json.RawMessage) (json.RawMessage, []string) {
	var p map[string]json.RawMessage
	if len(props) == 0 || json.Unmarshal(props, &p) != nil || p == nil {
		return props, nil
	}
	var dropped []string
	for k := range p {
		if !WidgetPropKeys[k] {
			dropped = append(dropped, k)
			delete(p, k)
		}
	}
	if raw, ok := p["chart"]; ok {
		var chart map[string]json.RawMessage
		if json.Unmarshal(raw, &chart) == nil && chart != nil {
			changed := false
			for k := range chart {
				if !ChartSettingKeys[k] {
					dropped = append(dropped, "chart."+k)
					delete(chart, k)
					changed = true
				}
			}
			if changed {
				p["chart"], _ = json.Marshal(chart)
			}
		}
	}
	if len(dropped) == 0 {
		return props, nil
	}
	sort.Strings(dropped)
	out, err := json.Marshal(p)
	if err != nil {
		return props, nil
	}
	return out, dropped
}

func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
