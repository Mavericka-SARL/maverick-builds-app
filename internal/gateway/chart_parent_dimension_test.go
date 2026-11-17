package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/query"
)

// The chart reader — charts, the chat connector's query_grid, workflow
// conditions — reads a territory metric at a region (the territory's parent
// dimension) as the territories under it: a SUMIFS-based one added up, a
// ratio over those sums. It read 0: SUMIFS(…, territory) evaluated at the
// region has no territory (Territory & Quota build, 2026-11-16).
func TestChartReaderReadsAChildDimensionMetricAtItsParent(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	rev := f.workingRevID
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
	}
	must := func(method, path string, body any) string {
		t.Helper()
		status, raw := call(method, path, body)
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		return out.ID
	}
	region := must("POST", "/api/developer/dimensions", map[string]any{"name": "sales_region", "revision_id": rev})
	eu := must("POST", "/api/developer/dimensions/"+region+"/members", map[string]any{"code": "EU", "label": "EU"})
	us := must("POST", "/api/developer/dimensions/"+region+"/members", map[string]any{"code": "US", "label": "US"})
	terr := must("POST", "/api/developer/dimensions", map[string]any{"name": "terr", "revision_id": rev, "parent_dimension_id": region})
	for code, parent := range map[string]string{"T1": eu, "T2": eu, "T3": us} {
		must("POST", "/api/developer/dimensions/"+terr+"/members", map[string]any{"code": code, "label": code, "parent_member_id": parent})
	}
	acct := must("POST", "/api/developer/dimensions", map[string]any{"name": "acct", "revision_id": rev})
	for _, c := range []string{"A1", "A2", "A3", "A4"} {
		must("POST", "/api/developer/dimensions/"+acct+"/members", map[string]any{"code": c, "label": c})
	}
	metric := func(body map[string]any) string {
		body["revision_id"] = rev
		return must("POST", "/api/developer/metrics", body)
	}
	grid := func(name, dim string, metrics ...string) {
		g := must("POST", "/api/developer/grids", map[string]any{"name": name, "revision_id": rev})
		must("POST", "/api/developer/grids/"+g+"/dimensions/"+dim, nil)
		for _, m := range metrics {
			must("POST", "/api/developer/grids/"+g+"/metrics/"+m, nil)
		}
	}
	accVal := metric(map[string]any{"name": "acc_val", "is_input": true})
	accTerr := metric(map[string]any{"name": "acc_terr", "is_input": true, "format": "picklist", "picklist_dimension_id": terr})
	grid("Accounts", acct, accVal, accTerr)
	tpSum := metric(map[string]any{"name": "tp_sum", "formula": "SUMIFS(acc_val, acc_terr, terr)"})
	tpCnt := metric(map[string]any{"name": "tp_cnt", "formula": "COUNTIFS(acc_terr, terr)"})
	tpAvg := metric(map[string]any{"name": "tp_avg", "formula": "IF(tp_cnt = 0, 0, tp_sum / tp_cnt)", "agg_rule": "formula"})
	// A region's pool shared out to its territories: the pool has a value
	// at a territory only (total rule none), the quotas add up.
	rpool := metric(map[string]any{"name": "rpool", "is_input": true})
	grid("Regions", region, rpool)
	tpPool := metric(map[string]any{"name": "tp_pool", "formula": "LOOKUP(rpool, sales_region, PARENT(terr))", "agg_rule": "none"})
	tpQ := metric(map[string]any{"name": "tp_q", "formula": "tp_pool * tp_sum / 100"})
	tpQr := metric(map[string]any{"name": "tp_qr", "formula": "IF(tp_sum = 0, 0, tp_q / tp_sum)", "agg_rule": "formula"})
	grid("Territories", terr, tpSum, tpCnt, tpAvg, tpPool, tpQ, tpQr)
	write := func(metricID, code string, body map[string]any) {
		t.Helper()
		body["model_id"], body["revision_id"], body["metric_id"] = f.modelID, rev, metricID
		body["dim_codes"] = map[string]string{acct: code}
		if status, raw := call("POST", "/api/cells", body); status != http.StatusOK {
			t.Fatalf("write: %d %s", status, raw)
		}
	}
	for code, w := range map[string]struct {
		v    float64
		terr string
	}{"A1": {10, "T1"}, "A2": {30, "T2"}, "A3": {50, "T3"}, "A4": {20, "T1"}} {
		write(accVal, code, map[string]any{"value": w.v})
		write(accTerr, code, map[string]any{"member": w.terr})
	}

	for code, v := range map[string]float64{"EU": 100, "US": 50} {
		body := map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": rpool, "value": v, "dim_codes": map[string]string{region: code}}
		if status, raw := call("POST", "/api/cells", body); status != http.StatusOK {
			t.Fatalf("write pool: %d %s", status, raw)
		}
	}
	r := query.NewChartResolver(f.pool)
	for code, want := range map[string]map[string]float64{
		"EU": {"tp_sum": 60, "tp_cnt": 3, "tp_avg": 20, "tp_q": 60, "tp_qr": 1},
		"US": {"tp_sum": 50, "tp_cnt": 1, "tp_avg": 50, "tp_q": 25, "tp_qr": 0.5},
	} {
		got, err := r.MetricValuesAt(ctx, f.modelID, rev, []string{"tp_sum", "tp_cnt", "tp_avg", "tp_q", "tp_qr"}, map[string]string{region: code})
		if err != nil {
			t.Fatal(err)
		}
		for name, w := range want {
			if n, ok := got[name].Number(); !ok || got[name].Kind() != formula.KindNumber || n != w {
				t.Errorf("%s at region %s = %v, want %v", name, code, got[name], w)
			}
		}
	}
	// The territory itself still reads its own value.
	got, err := r.MetricValuesAt(ctx, f.modelID, rev, []string{"tp_avg"}, map[string]string{terr: "T1"})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := got["tp_avg"].Number(); n != 15 {
		t.Errorf("tp_avg at T1 = %v, want 15", got["tp_avg"])
	}
}
