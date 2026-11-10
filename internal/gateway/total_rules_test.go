package gateway

// Total rules (migration 174; owner's decision 2026-11-09): a dimension's
// members total by the rule a developer sets on it — sum by default; a
// scenario dimension totals as its first member (Base) or not at all — and
// a metric may set its own rule for a dimension. Every total follows it: the
// KPI's totals (inputs and calculated), a one-dimension pin, the stored
// '{}' row; a revision copy carries both rules.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/query"
)

func TestDimensionTotalRules(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()
	post := func(path string, body any) (int, map[string]any) {
		t.Helper()
		return f.do(t, http.MethodPost, path, body)
	}
	patch := func(path string, body any) (int, map[string]any) {
		t.Helper()
		return f.do(t, http.MethodPatch, path, body)
	}
	mk := func(path string, body map[string]any) string {
		t.Helper()
		status, out := post(path, body)
		if status != http.StatusOK {
			t.Fatalf("POST %s: %d %v", path, status, out)
		}
		id, _ := out["id"].(string)
		return id
	}
	for _, code := range []string{"EU", "US"} {
		mk(fmt.Sprintf("/api/developer/dimensions/%s/members", f.dimID), map[string]any{"code": code, "label": code})
	}
	scenario := mk("/api/developer/dimensions", map[string]any{"name": "scenario", "revision_id": f.revID})
	for _, code := range []string{"BASE", "DOWN"} {
		mk(fmt.Sprintf("/api/developer/dimensions/%s/members", scenario), map[string]any{"code": code, "label": code})
	}
	units := mk("/api/developer/metrics", map[string]any{"name": "units", "is_input": true, "format": "number", "revision_id": f.revID})
	doubled := mk("/api/developer/metrics", map[string]any{"name": "doubled", "is_input": false, "formula": "units * 2", "format": "number", "revision_id": f.revID})
	grid := mk("/api/developer/grids", map[string]any{"name": "Plan", "revision_id": f.revID})
	for _, p := range []string{"/dimensions/" + f.dimID, "/dimensions/" + scenario, "/metrics/" + units, "/metrics/" + doubled} {
		if status, out := post("/api/developer/grids/"+grid+p, nil); status != http.StatusOK {
			t.Fatalf("assign %s: %d %v", p, status, out)
		}
	}
	if status, out := post("/api/import/upload", map[string]string{"revision_id": f.revID, "import_mode": "replace",
		"csv": "FixtureRegion,scenario,units\nEU,BASE,10\nUS,BASE,20\nEU,DOWN,1\nUS,DOWN,2\n"}); status != http.StatusOK {
		t.Fatalf("import: %d %v", status, out)
	}

	// The stored '{}' total of doubled, once the recalculation wrote want
	// (absent: no row).
	storedTotal := func(want float64, present bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			var v float64
			err := f.pool.QueryRow(ctx, `SELECT value::float8 FROM runtime.calc_result WHERE metric_id=$1::uuid AND dim_members='{}'::jsonb`, doubled).Scan(&v)
			if (present && err == nil && v == want) || (!present && err != nil) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("doubled's stored total = %v (%v), want %v (present %v)", v, err, want, present)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	totals := func(scope map[string]string) map[string]any {
		t.Helper()
		q := "/api/grid?totals_only=1&revision_id=" + f.revID
		if scope != nil {
			b, _ := json.Marshal(scope)
			q += "&scope=" + url.QueryEscape(string(b))
		}
		status, out := f.do(t, http.MethodGet, q, nil)
		if status != http.StatusOK {
			t.Fatalf("totals: %d %v", status, out)
		}
		got, _ := out["totals"].(map[string]any)
		return got
	}
	expect := func(what string, got map[string]any, id string, want float64, present bool) {
		t.Helper()
		v, ok := got[id].(float64)
		if ok != present || (present && v != want) {
			t.Errorf("%s: %v (present %v), want %v (present %v)", what, got[id], ok, want, present)
		}
	}

	// Sum, the default: the scenarios add up.
	storedTotal(66, true)
	tot := totals(nil)
	expect("units, sum", tot, units, 33, true)
	expect("doubled, sum", tot, doubled, 66, true)

	// Bad rules are refused, saying what a rule may be.
	if status, out := patch("/api/developer/dimensions/"+scenario, map[string]any{"agg_rule": "average"}); status != http.StatusBadRequest {
		t.Errorf("agg_rule average: %d %v, want 400", status, out)
	}
	if status, out := patch("/api/developer/metrics/"+units, map[string]any{"name": "units", "formula": "", "dimension_rules": map[string]string{"nowhere": "first"}}); status != http.StatusBadRequest {
		t.Errorf("a rule over no dimension: %d %v, want 400", status, out)
	}

	// first: every total is Base's, the regions still added up.
	if status, out := patch("/api/developer/dimensions/"+scenario, map[string]any{"agg_rule": "first"}); status != http.StatusOK {
		t.Fatalf("scenario first: %d %v", status, out)
	}
	storedTotal(60, true)
	tot = totals(nil)
	expect("units, first", tot, units, 30, true)
	expect("doubled, first", tot, doubled, 60, true)
	tot = totals(map[string]string{f.dimID: "EU"})
	expect("units at EU, first", tot, units, 10, true)
	expect("doubled at EU, first", tot, doubled, 20, true)
	tot = totals(map[string]string{scenario: "DOWN"})
	expect("units at Downside", tot, units, 3, true)

	// A chart by region without a scenario selector reads Base.
	var devID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM identity.user WHERE keycloak_sub='dev-audit-pa'`).Scan(&devID); err != nil {
		t.Fatal(err)
	}
	res, err := query.NewChartResolver(f.pool).Resolve(ctx, &query.ChartConfig{ChartType: query.ChartBar, DimensionID: f.dimID, MetricIDs: []string{units}},
		nil, f.modelID, f.revID, grid, devID)
	if err != nil {
		t.Fatalf("chart: %v", err)
	}
	chart, _ := res.(*query.CategoryChartData)
	var bars []float64
	if chart != nil && len(chart.Series) == 1 {
		for _, v := range chart.Series[0].Values {
			if v != nil {
				bars = append(bars, *v)
			}
		}
	}
	if len(bars) != 2 || bars[0] != 10 || bars[1] != 20 {
		t.Errorf("chart by region, first: %v, want [10 20] (Base)", bars)
	}

	// A metric's own rule wins: doubled adds the scenarios up.
	if status, out := patch("/api/developer/metrics/"+doubled, map[string]any{"name": "doubled", "formula": "units * 2", "dimension_rules": map[string]string{"scenario": "sum"}}); status != http.StatusOK {
		t.Fatalf("doubled's own rule: %d %v", status, out)
	}
	storedTotal(66, true)
	tot = totals(nil)
	expect("units, first", tot, units, 30, true)
	expect("doubled, its own sum", tot, doubled, 66, true)

	// A revision copy carries both rules.
	next := mk("/api/developer/revisions", map[string]any{"name": "Next", "source_revision_id": f.revID})
	var copied string
	if err := f.pool.QueryRow(ctx, `
		SELECT d.agg_rule || '/' || COALESCE((SELECT r.rule FROM model.metric_dimension_rule r JOIN model.metric_def m ON m.id = r.metric_id
		                                       WHERE m.revision_id = $1::uuid AND m.name = 'doubled' AND r.dimension_id = d.id), '')
		FROM model.dimension_def d WHERE d.revision_id = $1::uuid AND d.name = 'scenario'`, next).Scan(&copied); err != nil || copied != "first/sum" {
		t.Errorf("the copy's rules: %q (%v), want first/sum", copied, err)
	}

	// none: no total over the scenarios; one scenario still has its own.
	if status, out := patch("/api/developer/metrics/"+doubled, map[string]any{"name": "doubled", "formula": "units * 2", "dimension_rules": map[string]string{}}); status != http.StatusOK {
		t.Fatalf("drop doubled's rule: %d %v", status, out)
	}
	if status, out := patch("/api/developer/dimensions/"+scenario, map[string]any{"agg_rule": "none"}); status != http.StatusOK {
		t.Fatalf("scenario none: %d %v", status, out)
	}
	storedTotal(0, false)
	tot = totals(nil)
	expect("units, none", tot, units, 0, false)
	expect("doubled, none", tot, doubled, 0, false)
	tot = totals(map[string]string{scenario: "BASE"})
	expect("units at Base, none", tot, units, 30, true)

	var stored string
	if err := f.pool.QueryRow(ctx, `SELECT agg_rule FROM model.dimension_def WHERE id=$1::uuid`, scenario).Scan(&stored); err != nil || stored != "none" {
		t.Errorf("the scenario's stored rule: %q (%v)", stored, err)
	}
}
