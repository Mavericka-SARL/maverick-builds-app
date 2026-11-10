package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
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

// A change to a dimension recalculates the revision the dimension belongs
// to, and no other: every revision of the model holding data used to be
// recalculated, approved ones included (OBSERVATIONS "A time-dimension or
// member change recalculates every revision of the model that holds data").
// The pass runs here synchronously; the routes start it in a goroutine.
func TestDimensionChangeRecalculatesOnlyItsRevision(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()
	month := f.call("POST", "/api/developer/dimensions", map[string]any{
		"name": "month", "revision_id": f.revID, "dimension_type": "time",
		"time_granularity": "month", "fiscal_year_start_month": 1,
	})
	f.call("POST", "/api/developer/dimensions/"+month+"/members", map[string]any{"code": "2026-01", "label": "Jan", "period_start": "2026-01-01", "period_end": "2026-01-31"})
	f.metric["double"] = f.call("POST", "/api/developer/metrics", f.metricBody("double", `revenue * 2`))
	f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["double"], nil)
	f.writeCell("revenue", "US", 50)
	f.await("double", "US", 100)

	// Another revision of the model, holding the same data.
	next := f.call("POST", "/api/developer/revisions", map[string]any{"name": "Next", "source_revision_id": f.revID})
	if next == "" {
		t.Fatal("the revision copy answered no id")
	}
	doubleIn := func(rev string) (float64, bool) {
		var v float64
		err := f.pool.QueryRow(ctx, `
			SELECT cr.value FROM runtime.calc_result cr JOIN model.metric_def m ON m.id = cr.metric_id
			WHERE cr.revision_id = $1::uuid AND m.name = 'double'
			  AND EXISTS (SELECT 1 FROM jsonb_each_text(cr.dim_members) e WHERE e.value = 'US')
			ORDER BY cr.calc_at DESC LIMIT 1`, rev).Scan(&v)
		return v, err == nil
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if v, ok := doubleIn(next); ok && nearly(v, 100) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the copied revision never computed double")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // the copy's own recalculation settles

	// Both revisions' stored results go stale; the pass for the first
	// revision's dimension restores the first only.
	if _, err := f.pool.Exec(ctx, `UPDATE runtime.calc_result SET value = 999 WHERE model_id = $1::uuid
		AND metric_id IN (SELECT id FROM model.metric_def WHERE model_id = $1::uuid AND name = 'double')`, f.modelID); err != nil {
		t.Fatal(err)
	}
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(f.pool, nil)}
	h.recalcDimensionRevision(ctx, f.modelID, month)
	if v, _ := doubleIn(f.revID); !nearly(v, 100) {
		t.Errorf("double in the dimension's revision = %v after the pass, want 100", v)
	}
	if v, _ := doubleIn(next); v != 999 {
		t.Errorf("double in the other revision = %v after the pass, want it untouched (999)", v)
	}
}
