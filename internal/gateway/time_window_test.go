package gateway

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

// A rolling window: the periods from the one holding a metric's date, and
// some before it; a date between periods starts at the next; no date, no
// window (2026-11-20: a 13-week forecast on a dated week calendar).
func TestTimeWindowFollowsTheDate(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := d.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	week := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid, $2::uuid, 'week', 'time', 'week', 1) RETURNING id::text`, d.modelID, d.revID)
	for i, start := range []string{"2026-11-02", "2026-11-09", "2026-11-16", "2026-11-23", "2026-11-30"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index)
			VALUES ($1::uuid, $2::text, $2::text, $2::text::date, $2::text::date + 6, $3) RETURNING id::text`, week, start, i)
	}
	first := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format) VALUES ($1::uuid, $2::uuid, 'first_week', true, 'date') RETURNING id::text`, d.modelID, d.revID)
	window := func(periods, before string) []string {
		t.Helper()
		v := url.Values{"revision_id": {d.revID}, "dimension_id": {week}, "from_metric_id": {first}, "periods": {periods}, "before": {before}}
		res := d.call("GET", "/api/time-window?"+v.Encode(), d.admin, nil)
		raw, _ := res["codes"].([]any)
		if res["codes"] == nil {
			return nil
		}
		out := []string{}
		for _, c := range raw {
			out = append(out, c.(string))
		}
		return out
	}
	if got := window("2", "0"); got != nil {
		t.Errorf("no date yet: %v, want no window", got)
	}
	set := func(serial int) {
		t.Helper()
		q(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value) VALUES ($1::uuid, $2::uuid, $3::uuid, '{}', $4) RETURNING id::text`,
			d.modelID, d.revID, first, serial)
	}
	set(46342) // 2026-11-16
	if got, want := window("2", "1"), []string{"2026-11-09", "2026-11-16", "2026-11-23"}; !reflect.DeepEqual(got, want) {
		t.Errorf("from 16-Nov, 2 periods and 1 before: %v, want %v", got, want)
	}
	set(46350) // 2026-11-24, inside the week of 23-Nov
	if got, want := window("13", "0"), []string{"2026-11-23", "2026-11-30"}; !reflect.DeepEqual(got, want) {
		t.Errorf("from 24-Nov, 13 periods: %v, want the two the calendar has, %v", got, want)
	}
	v := url.Values{"revision_id": {d.revID}, "dimension_id": {week}, "from_metric_id": {first}, "periods": {"0"}}
	if status, raw := d.req("GET", "/api/time-window?"+v.Encode(), d.admin, nil); status != http.StatusBadRequest {
		t.Errorf("0 periods: %d %s, want 400", status, raw)
	}
}
