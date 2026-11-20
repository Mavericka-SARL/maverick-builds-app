package metricformula

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// A metric with a time function on one calendar may read a metric of another
// calendar through a LOOKUP that names a member of that calendar — one value
// per cell, no mapping between the calendars — but not by name
// (2026-11-20: TIMESUM(credit) / days * LOOKUP(drv_days, week, …) on a
// forecast-week axis).
func TestLookupIntoAnotherCalendarBesideATimeFunction(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	fweek := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid, $2::uuid, 'fweek', 'time', 'week', 1) RETURNING id::text`, f.modelID, f.revID)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index)
		VALUES ($1::uuid, 'W01', 'Week 1', '2026-11-16', '2026-11-22', 0) RETURNING id::text`, fweek)
	g3 := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'G3') RETURNING id::text`, f.modelID, f.revID)
	q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid) RETURNING grid_id::text`, g3, fweek)
	flow := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'wflow', true) RETURNING id::text`, f.modelID, f.revID)
	target := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula) VALUES ($1::uuid, $2::uuid, 'wtarget', false, '0') RETURNING id::text`, f.modelID, f.revID)
	for _, m := range []string{flow, target} {
		q(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid) RETURNING grid_id::text`, g3, m)
	}
	set := func(text string) error {
		t.Helper()
		if _, err := f.pool.Exec(ctx, `UPDATE model.metric_def SET formula = $2 WHERE id = $1::uuid`, target, text); err != nil {
			t.Fatal(err)
		}
		return ValidateGridTime(ctx, f.pool, f.modelID, f.revID, g3)
	}
	if err := set(`TIMESUM(wflow) * LOOKUP(revenue, month, "2026-01", region, "EMEA")`); err != nil {
		t.Errorf("a LOOKUP naming the other calendar's member beside TIMESUM is refused: %v", err)
	}
	if err := set(`TIMESUM(wflow) * LOOKUP(revenue, region, "EMEA")`); codeOf(err) != formula.CodeTimeDimensionMismatch {
		t.Errorf("a LOOKUP leaving the other calendar open: want %s, got %v", formula.CodeTimeDimensionMismatch, err)
	}
	if err := set(`TIMESUM(wflow) * revenue`); codeOf(err) != formula.CodeTimeDimensionMismatch {
		t.Errorf("reading the other calendar's metric by name: want %s, got %v", formula.CodeTimeDimensionMismatch, err)
	}
}
