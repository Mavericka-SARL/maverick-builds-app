package gateway

import (
	"context"
	"testing"
)

// A period added to a time dimension recalculates the metrics that read the
// period set in every revision — a revision with no typed value included. The
// time branches used to recalculate only the revisions holding fact rows, so
// COUNTIFS(month, "*") in a revision with no data kept the old count
// (OBSERVATIONS "A time-dimension member change recalculates only revisions
// with fact data").
func TestTimeMemberChangeRecalculatesARevisionWithoutFacts(t *testing.T) {
	f := setupDimFormulaFixture(t)
	month := f.call("POST", "/api/developer/dimensions", map[string]any{
		"name": "month", "revision_id": f.revID, "dimension_type": "time",
		"time_granularity": "month", "fiscal_year_start_month": 1,
	})
	members := "/api/developer/dimensions/" + month + "/members"
	f.call("POST", members, map[string]any{"code": "2026-01", "label": "Jan", "period_start": "2026-01-01", "period_end": "2026-01-31"})
	f.call("POST", members, map[string]any{"code": "2026-02", "label": "Feb", "period_start": "2026-02-01", "period_end": "2026-02-28"})
	f.metric["n_months"] = f.call("POST", "/api/developer/metrics", f.metricBody("n_months", `COUNTIFS(month, "*")`))
	f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["n_months"], nil)
	f.call("POST", "/api/developer/revisions/"+f.revID+"/recalculate", nil)
	f.await("n_months", "US", 2)

	// Added one at a time.
	f.call("POST", members, map[string]any{"code": "2026-03", "label": "Mar", "period_start": "2026-03-01", "period_end": "2026-03-31"})
	f.await("n_months", "US", 3)

	// Generated.
	f.call("POST", members+"/generate", map[string]any{"start": "2026-04-01", "end": "2026-05-31"})
	f.await("n_months", "US", 5)

	// Imported.
	f.call("POST", "/api/import/dimension-members", map[string]any{
		"dimension_id": month,
		"csv":          "code,label,period_start,period_end\n2026-06,Jun,2026-06-01,2026-06-30\n",
	})
	f.await("n_months", "US", 6)

	var facts int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, f.modelID).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 0 {
		t.Fatalf("fact rows = %d; the test needs a revision without facts", facts)
	}
}
