package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// A rolling period window: a grid's or chart's time dimension shown from the
// period holding a date a metric holds — the first forecast week, the last
// closed month — for N periods, optionally some before it. When the date
// moves the window moves; a fixed member list (show_members, plotted_members)
// had to be edited every week, so the 13-week cash model calculated on a
// second "Week 1 … Week 13" dimension with placeholder dates (2026-11-20).
// widget_props.time_window: {"dimension_id", "from_metric_id", "periods",
// "before"}; members listed in show_members / chart.plotted_members besides
// the window's (a total) are shown after it.

type timeWindow struct {
	DimensionID  string `json:"dimension_id"`
	FromMetricID string `json:"from_metric_id"`
	Periods      int    `json:"periods"`
	Before       int    `json:"before,omitempty"`
}

// maxWindowPeriods bounds a window.
const maxWindowPeriods = 400

// windowCodes is the window's leaf periods, in order: the period whose dates
// hold (or, the date falling between periods, follow) the metric's date, the
// `before` periods ahead of it and periods-1 after it. No date yet (the
// metric is empty) is no window: nil, and the widget shows every period.
func (h *handler) windowCodes(ctx context.Context, revisionID string, w timeWindow) ([]string, error) {
	if w.DimensionID == "" || w.FromMetricID == "" {
		return nil, errors.New("a time window names its time dimension and the metric holding its start date")
	}
	if w.Periods < 1 || w.Periods > maxWindowPeriods || w.Before < 0 || w.Before > maxWindowPeriods {
		return nil, fmt.Errorf("a time window shows 1 to %d periods, and 0 to %d before its start", maxWindowPeriods, maxWindowPeriods)
	}
	// The date: the metric's latest typed value, else its calculated one, at
	// no dimension (a setting such as the first forecast week).
	var serial *float64
	err := h.db.QueryRow(ctx, `
		SELECT value FROM runtime.fact_input
		WHERE metric_id = $1::uuid AND revision_id = $2::uuid AND dim_members = '{}'::jsonb
		ORDER BY entered_at DESC, id DESC LIMIT 1`, w.FromMetricID, revisionID).Scan(&serial)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && serial == nil) {
		err = h.db.QueryRow(ctx, `
			SELECT value FROM runtime.calc_result
			WHERE metric_id = $1::uuid AND revision_id = $2::uuid AND dim_members = '{}'::jsonb
			ORDER BY calc_at DESC LIMIT 1`, w.FromMetricID, revisionID).Scan(&serial)
	}
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && serial == nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	day := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(*serial))
	rows, err := h.db.Query(ctx, `
		SELECT code, period_end FROM model.dimension_member
		WHERE dimension_id = $1::uuid AND period_start IS NOT NULL
		ORDER BY period_start, code`, w.DimensionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var codes []string
	at := -1
	for rows.Next() {
		var code string
		var end time.Time
		if err := rows.Scan(&code, &end); err != nil {
			return nil, err
		}
		if at < 0 && !end.Before(day) {
			at = len(codes)
		}
		codes = append(codes, code)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if at < 0 {
		return []string{}, nil // the date is after every period
	}
	from, to := max(0, at-w.Before), min(len(codes), at+w.Periods)
	return codes[from:to], nil
}

// timeWindowCodes handles GET /api/time-window?revision_id=&dimension_id=&
// from_metric_id=&periods=&before= — the window's period codes a grid
// widget shows; {"codes": null} while the date is empty.
func (h *handler) timeWindowCodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	revisionID := q.Get("revision_id")
	// The revision, the dimension and the metric are one model's, and the
	// caller may open it.
	var modelID string
	if err := h.db.QueryRow(ctx, `
		SELECT d.model_id::text FROM model.dimension_def d JOIN model.metric_def m ON m.model_id = d.model_id
		WHERE d.id::text = $1 AND m.id::text = $2 AND d.revision_id::text = $3 AND m.revision_id::text = $3`,
		q.Get("dimension_id"), q.Get("from_metric_id"), revisionID).Scan(&modelID); err != nil {
		jsonErr(w, fmt.Errorf("no such dimension and metric in this revision"), http.StatusNotFound)
		return
	}
	if can, err := h.actorCanAccessModel(ctx, act, modelID); err != nil || !can {
		jsonErr(w, fmt.Errorf("forbidden: model is outside your access scope"), http.StatusForbidden)
		return
	}
	periods, _ := strconv.Atoi(q.Get("periods"))
	before, _ := strconv.Atoi(q.Get("before"))
	codes, err := h.windowCodes(ctx, revisionID, timeWindow{DimensionID: q.Get("dimension_id"), FromMetricID: q.Get("from_metric_id"), Periods: periods, Before: before})
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"codes": codes})
}

// windowedMembers is a widget's members on the window's dimension: the
// window's periods, then the members listed besides them (a total).
func windowedMembers(window, listed []string) []string {
	out := append([]string{}, window...)
	in := map[string]bool{}
	for _, c := range window {
		in[c] = true
	}
	for _, c := range listed {
		if !in[c] {
			out = append(out, c)
		}
	}
	return out
}

// widgetTimeWindow reads widget_props.time_window, nil when there is none.
func widgetTimeWindow(props []byte) *timeWindow {
	var p struct {
		TimeWindow *timeWindow `json:"time_window"`
	}
	if json.Unmarshal(props, &p) != nil {
		return nil
	}
	return p.TimeWindow
}
