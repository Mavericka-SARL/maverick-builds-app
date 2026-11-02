package modeledit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// The dashboard canvas the console lays widgets out on: rows of widgets from
// the top, each row's widgets side by side. A widget's place is pixels; the
// console rounds both edges to 20px columns.
const (
	CanvasWidth = 1100
	WidgetGap   = 20
	columnPx    = 20
	// A grid taller than this scrolls inside its box instead.
	maxGridHeight = 640
)

// LayoutError is a wrong arrangement: a widget of another dashboard, one
// listed twice or left out.
type LayoutError struct{ Msg string }

func (e *LayoutError) Error() string { return e.Msg }

// ArrangeDashboard lays a dashboard's widgets out — POST
// /api/developer/dashboards/{id}/arrange and the AI Developer's
// arrange_dashboard. rows lists every widget of the dashboard once, row by row
// from the top; a row's widgets stand side by side, left to right, sharing
// the canvas width by weights (the same shape as rows; left out, equal) with
// 20px between them, and the rows stack 20px apart. Each widget is as tall as
// what it shows (WidgetHeight). It returns the number of widgets placed.
func ArrangeDashboard(ctx context.Context, db DB, dashboardID string, rows [][]string, weights [][]float64) (int, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	type widget struct {
		typ, title, ref, content string
		props                    json.RawMessage
		sizeH                    int
	}
	widgets := map[string]widget{}
	q, err := tx.Query(ctx, `SELECT id::text, widget_type, COALESCE(title, ''), COALESCE(ref_id::text, ''),
		COALESCE(content, ''), COALESCE(widget_props, '{}'::jsonb), COALESCE(size_h, 0)
		FROM model.dashboard_widget WHERE dashboard_id = $1::uuid FOR UPDATE`, dashboardID)
	if err != nil {
		return 0, err
	}
	for q.Next() {
		var id string
		var w widget
		if err := q.Scan(&id, &w.typ, &w.title, &w.ref, &w.content, &w.props, &w.sizeH); err != nil {
			q.Close()
			return 0, err
		}
		widgets[id] = w
	}
	q.Close()
	if err := q.Err(); err != nil {
		return 0, err
	}

	seen := map[string]bool{}
	for _, row := range rows {
		if len(row) == 0 {
			return 0, &LayoutError{"a row holds no widget"}
		}
		for _, raw := range row {
			id := strings.ToLower(strings.TrimSpace(raw))
			w, ok := widgets[id]
			if !ok {
				return 0, &LayoutError{fmt.Sprintf("widget %s is not on this dashboard", raw)}
			}
			if seen[id] {
				return 0, &LayoutError{fmt.Sprintf("the %s widget %s is listed twice", w.typ, describeWidget(id, w.title))}
			}
			seen[id] = true
		}
	}
	if len(seen) != len(widgets) {
		var missing []string
		for id, w := range widgets {
			if !seen[id] {
				missing = append(missing, w.typ+" "+describeWidget(id, w.title))
			}
		}
		return 0, &LayoutError{"every widget of the dashboard is placed once; missing: " + strings.Join(missing, ", ")}
	}

	y := 0
	for i, row := range rows {
		total := 0.0
		weight := func(j int) float64 {
			if i < len(weights) && j < len(weights[i]) && weights[i][j] > 0 {
				return weights[i][j]
			}
			return 1
		}
		for j := range row {
			total += weight(j)
		}
		free := float64(CanvasWidth - WidgetGap*(len(row)-1))
		x, rowH := 0, 0
		for j, raw := range row {
			id := strings.ToLower(strings.TrimSpace(raw))
			w := widgets[id]
			width := int(math.Floor(free*weight(j)/total/columnPx)) * columnPx
			width = max(width, columnPx)
			h := WidgetHeight(ctx, tx, w.typ, w.ref, w.props, w.content)
			if h == 0 {
				h = cmpOr(w.sizeH, 360)
			}
			if _, err := tx.Exec(ctx, `UPDATE model.dashboard_widget SET pos_x=$2, pos_y=$3, size_w=$4, size_h=$5,
				col_start=$6, col_span=$7 WHERE id=$1::uuid`, id, x, y, width, h, x/columnPx+1, width/columnPx); err != nil {
				return 0, err
			}
			x += width + WidgetGap
			rowH = max(rowH, h)
		}
		y += rowH + WidgetGap
	}
	return len(widgets), tx.Commit(ctx)
}

func describeWidget(id, title string) string {
	if title != "" {
		return fmt.Sprintf("%q (%s)", title, id)
	}
	return id
}

func cmpOr(a, b int) int {
	if a > 0 {
		return a
	}
	return b
}

// WidgetHeight is the height a widget needs for what it shows: a KPI tile
// 120, a button 40, a chart 360, a records table or board 420, a grid its
// toolbar, header rows and rows (up to 640, taller scrolls inside), a text
// its lines and a workflow's status its steps (both grow to their content on
// the page anyway). 0: the widget keeps its own (an image).
func WidgetHeight(ctx context.Context, q Querier, widgetType, refID string, props json.RawMessage, content string) int {
	switch widgetType {
	case "metric_kpi":
		return 120
	case "automation_button", "integration_button":
		return 40
	case "chart":
		return 360
	case "form", "board":
		return 540
	case "grid":
		return gridWidgetHeight(ctx, q, refID, props)
	case "text":
		return textHeight(content)
	case "workflow_status":
		return workflowStatusHeight(ctx, q, refID)
	}
	return 0
}

const metricsAxis = "__metrics__"

// gridWidgetHeight: the toolbar, a header row per level of each column
// dimension (a year, its quarters and months are three), the metric header
// when metrics are columns, a row per row member (per metric when metrics are
// rows), and the legend under the grid. A dimension the grid does not have
// is left out, as the console leaves it out.
func gridWidgetHeight(ctx context.Context, q Querier, gridID string, raw json.RawMessage) int {
	var p struct {
		DefaultView *struct {
			Rows []string `json:"rows"`
			Cols []string `json:"cols"`
		} `json:"default_view"`
		MetricIDs     []string            `json:"metric_ids"`
		ShowMembers   map[string][]string `json:"show_members"`
		RowsCollapsed bool                `json:"rows_collapsed"`
	}
	_ = json.Unmarshal(raw, &p)
	metrics := len(p.MetricIDs)
	if metrics == 0 && gridID != "" {
		_ = q.QueryRow(ctx, `SELECT count(*) FROM model.grid_metric WHERE grid_id = $1::uuid`, gridID).Scan(&metrics)
	}
	metrics = max(metrics, 1)
	onGrid := gridDimensions(ctx, q, gridID)
	rows, headers := metrics, 2
	if p.DefaultView != nil {
		rows, headers = 1, 0
		rowDims := 0
		for _, d := range p.DefaultView.Rows {
			if d == metricsAxis {
				rows *= metrics
				rowDims++
				continue
			}
			if !onGrid[d] {
				continue
			}
			rows *= memberRows(ctx, q, d, p.ShowMembers[d], p.RowsCollapsed)
			rowDims++
		}
		if rowDims == 0 && len(p.DefaultView.Cols) == 1 && p.DefaultView.Cols[0] == metricsAxis {
			// no rows and only the metrics as columns: the console lists them
			// down the grid, "Metric | Value", a row each
			rows = metrics // the cols loop below counts its one header row
		}
		for _, d := range p.DefaultView.Cols {
			if d == metricsAxis {
				headers++
				continue
			}
			if !onGrid[d] {
				continue
			}
			if len(p.ShowMembers[d]) > 0 {
				headers++
				continue
			}
			headers += dimensionDepth(ctx, q, d)
		}
		if !contains(p.DefaultView.Cols, metricsAxis) {
			headers++ // the metric's name over the values, or the corner label
		}
	}
	h := 56 + headers*40 + rows*37 + 34 + 16
	return min(max(h, 160), maxGridHeight)
}

// gridDimensions: the grid's dimensions, by id and by name.
func gridDimensions(ctx context.Context, q Querier, gridID string) map[string]bool {
	var ids, names []string
	_ = q.QueryRow(ctx, `SELECT COALESCE(array_agg(gd.dimension_id::text), '{}'), COALESCE(array_agg(d.name), '{}')
		FROM model.grid_dimension gd JOIN model.dimension_def d ON d.id = gd.dimension_id
		WHERE gd.grid_id::text = $1`, gridID).Scan(&ids, &names)
	out := map[string]bool{}
	for _, v := range append(ids, names...) {
		out[v] = true
	}
	return out
}

// CheckGridWidgetView refuses a grid widget's default_view naming a
// dimension its grid does not have: the console leaves it out and draws the
// grid as one "All" row (the AI build's Variance page, 2026-11-01).
func CheckGridWidgetView(ctx context.Context, q Querier, gridID string, widgetProps json.RawMessage) error {
	var p struct {
		DefaultView *struct {
			Rows    []string `json:"rows"`
			Cols    []string `json:"cols"`
			Context []string `json:"context"`
		} `json:"default_view"`
	}
	if json.Unmarshal(widgetProps, &p) != nil || p.DefaultView == nil || gridID == "" {
		return nil
	}
	onGrid := gridDimensions(ctx, q, gridID)
	var gridName string
	var names []string
	_ = q.QueryRow(ctx, `SELECT g.name, COALESCE(array_agg(d.name ORDER BY d.name) FILTER (WHERE d.name IS NOT NULL), '{}')
		FROM model.grid_def g LEFT JOIN model.grid_dimension gd ON gd.grid_id = g.id
		LEFT JOIN model.dimension_def d ON d.id = gd.dimension_id
		WHERE g.id::text = $1 GROUP BY g.name`, gridID).Scan(&gridName, &names)
	for _, d := range append(append(append([]string{}, p.DefaultView.Rows...), p.DefaultView.Cols...), p.DefaultView.Context...) {
		if d == metricsAxis || onGrid[d] {
			continue
		}
		label := d
		_ = q.QueryRow(ctx, `SELECT name FROM model.dimension_def WHERE id::text = $1`, d).Scan(&label)
		return fmt.Errorf("default_view names dimension %q, which grid %q does not have (its dimensions: %s; %q puts the metrics on an axis)",
			label, gridName, strings.Join(names, ", "), metricsAxis)
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// memberRows: the rows a dimension gives the grid — the members chosen, else
// every member (parents are rows too), only the top level when rows start
// collapsed.
func memberRows(ctx context.Context, q Querier, dimID string, shown []string, collapsed bool) int {
	if len(shown) > 0 {
		return len(shown)
	}
	var n int
	sql := `SELECT count(*) FROM model.dimension_member WHERE dimension_id = $1::uuid`
	if collapsed {
		sql += ` AND COALESCE(parent_member_id, parent_id) IS NULL`
	}
	if err := q.QueryRow(ctx, sql, dimID).Scan(&n); err != nil || n == 0 {
		return 1
	}
	return n
}

// dimensionDepth: how many levels a dimension's hierarchy has (1 for a flat one).
func dimensionDepth(ctx context.Context, q Querier, dimID string) int {
	var depth int
	err := q.QueryRow(ctx, `
		WITH RECURSIVE t AS (
			SELECT id, 1 AS lvl FROM model.dimension_member
			WHERE dimension_id = $1::uuid AND COALESCE(parent_member_id, parent_id) IS NULL
			UNION ALL
			SELECT m.id, t.lvl + 1 FROM model.dimension_member m JOIN t ON COALESCE(m.parent_member_id, m.parent_id) = t.id
			WHERE t.lvl < 8)
		SELECT COALESCE(max(lvl), 1) FROM t`, dimID).Scan(&depth)
	if err != nil || depth < 1 {
		return 1
	}
	return depth
}

// textHeight: a heading's line, a list item's, a paragraph's lines at about
// 140 characters each, the gaps between blocks.
func textHeight(content string) int {
	h := 16
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			h += 8
		case strings.HasPrefix(t, "# "):
			h += 40
		case strings.HasPrefix(t, "#"):
			h += 34
		default:
			h += 24 * max(1, (len([]rune(t))+139)/140)
		}
	}
	return max(h, 40)
}

// workflowStatusHeight: the card's header and a line per step of the
// workflow (its ref is the workflow's name or id).
func workflowStatusHeight(ctx context.Context, q Querier, ref string) int {
	var steps int
	_ = q.QueryRow(ctx, `SELECT COALESCE(jsonb_array_length(CASE WHEN jsonb_typeof(steps) = 'array' THEN steps
		ELSE COALESCE(steps->'steps', '[]'::jsonb) END), 0)
		FROM workflow.workflow_def WHERE id::text = $1 OR name = $1 ORDER BY updated_at DESC LIMIT 1`, ref).Scan(&steps)
	return 100 + 24*max(steps, 3)
}
