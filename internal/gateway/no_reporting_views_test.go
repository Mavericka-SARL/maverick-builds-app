package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// The per-model reporting views (runtime.m_<model>_inputs/_results) were
// removed by migration 143: nothing read them, and the inputs view summed
// every correction of a cell. They were rebuilt after each of the four model
// changes below; none of them may build one again, nor log generated SQL in
// deployment.schema_migration.
func TestModelChangesBuildNoReportingViews(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	must := func(method, path string, body any) string {
		t.Helper()
		status, out := f.do(t, method, path, dev, body)
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s %s: %d %v", method, path, status, out)
		}
		id, _ := out["id"].(string)
		return id
	}

	must("POST", "/api/developer/dimensions", map[string]any{"name": "channel", "revision_id": f.workingRevID})
	units := must("POST", "/api/developer/metrics", map[string]any{"name": "units", "is_input": true, "revision_id": f.workingRevID})
	must("DELETE", "/api/developer/metrics/"+units, nil)
	must("PUT", "/api/developer/revisions/"+f.annualRevID+"/activate", nil)
	// The views were built in a goroutine after the response; give any such
	// work time to land before looking.
	time.Sleep(time.Second)

	var views int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_views
		WHERE schemaname = 'runtime' AND viewname ~ '^m_[0-9a-f]+_(inputs|results)$'`).Scan(&views); err != nil {
		t.Fatal(err)
	}
	if views != 0 {
		t.Errorf("%d per-model reporting views exist after model changes, want none", views)
	}
	var logTable *string
	if err := f.pool.QueryRow(ctx, `SELECT to_regclass('deployment.schema_migration')::text`).Scan(&logTable); err != nil {
		t.Fatal(err)
	}
	if logTable != nil {
		t.Errorf("deployment.schema_migration exists, want it dropped")
	}
}
