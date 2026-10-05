package gateway

// Calculated members: a member of a standard dimension computed from the
// others, for every metric (migration 109). A comparison layout — RF, LY,
// Variance and Variance % of a P&L — used to take one metric per line per
// block (52 metrics on four grids for a 13-line P&L); with a Scenario
// dimension it is one metric per line. Driven through the developer API.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCalculatedMembers(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
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
	const varPct = `IF(METRICFORMAT() = "percentage", {RF} - {LY}, IF({LY} = 0, 0, ({RF} - {LY}) / ABS({LY}) * 100))`

	scen := must("POST", "/api/developer/dimensions", map[string]any{"name": "scenario", "revision_id": f.workingRevID})
	members := "/api/developer/dimensions/" + scen + "/members"
	must("POST", members, map[string]any{"code": "RF", "label": "Rolling forecast"})
	ly := must("POST", members, map[string]any{"code": "LY", "label": "Prior year"})
	varID := must("POST", members, map[string]any{"code": "VAR", "label": "Variance", "formula": "{RF} - {LY}"})
	must("POST", members, map[string]any{"code": "VARPCT", "label": "Variance %", "formula": varPct})
	mon := must("POST", "/api/developer/dimensions", map[string]any{"name": "month", "revision_id": f.workingRevID})
	fy := must("POST", "/api/developer/dimensions/"+mon+"/members", map[string]any{"code": "FY", "label": "FY"})
	must("POST", "/api/developer/dimensions/"+mon+"/members", map[string]any{"code": "M1", "label": "Jan", "parent_member_id": fy})
	must("POST", "/api/developer/dimensions/"+mon+"/members", map[string]any{"code": "M2", "label": "Feb", "parent_member_id": fy})

	sales := must("POST", "/api/developer/metrics", map[string]any{"name": "sales", "is_input": true, "format": "currency", "revision_id": f.workingRevID})
	margin := must("POST", "/api/developer/metrics", map[string]any{"name": "margin", "formula": "sales / 10", "format": "percentage", "agg_rule": "formula", "revision_id": f.workingRevID})
	grid := must("POST", "/api/developer/grids", map[string]any{"name": "P&L", "revision_id": f.workingRevID})
	for _, p := range []string{"/metrics/" + sales, "/metrics/" + margin, "/dimensions/" + scen, "/dimensions/" + mon} {
		must("POST", "/api/developer/grids/"+grid+p, nil)
	}
	write := func(scenario, month string, v float64) (int, string) {
		return call("POST", "/api/cells", map[string]any{"model_id": f.modelID, "revision_id": f.workingRevID, "metric_id": sales,
			"dim_codes": map[string]string{scen: scenario, mon: month}, "value": v})
	}
	for _, w := range []struct {
		s, m string
		v    float64
	}{{"RF", "M1", 120}, {"RF", "M2", 90}, {"LY", "M1", 100}, {"LY", "M2", 60}} {
		if status, raw := write(w.s, w.m, w.v); status != http.StatusOK {
			t.Fatalf("write %s/%s: %d %s", w.s, w.m, status, raw)
		}
	}
	// A calculated member takes no input.
	if status, raw := write("VAR", "M1", 5); status == http.StatusOK || !strings.Contains(raw, "calculated member") {
		t.Errorf("a write at VAR: %d %s, want it refused", status, raw)
	}

	type gridResp struct {
		Cells  map[string]float64 `json:"cells"`
		Totals map[string]float64 `json:"totals"`
	}
	read := func(query string) gridResp {
		t.Helper()
		var g gridResp
		// The calculation of margin runs after the writes; poll briefly.
		for i := 0; i < 40; i++ {
			_, raw := call("GET", "/api/grid?grid_id="+grid+query, nil)
			g = gridResp{}
			_ = json.Unmarshal([]byte(raw), &g)
			if _, ok := g.Totals[margin]; ok || strings.Contains(query, "scope") {
				break
			}
		}
		return g
	}
	near := func(what string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-6 {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	waitCalc(t, f, margin)
	g := read("")
	// Leaf cells: the formula over the siblings at the same coordinate (key
	// order: the metric's own dimensions as the grid lists them).
	key := func(metric, scenario, month string) string {
		for _, k := range []string{metric + ":" + scenario + ":" + month, metric + ":" + month + ":" + scenario} {
			if _, ok := g.Cells[k]; ok {
				return k
			}
		}
		return metric + ":" + scenario + ":" + month
	}
	near("sales VAR M1", g.Cells[key(sales, "VAR", "M1")], 20)
	near("sales VARPCT M2", g.Cells[key(sales, "VARPCT", "M2")], 50)
	near("margin VAR M1 (points)", g.Cells[key(margin, "VAR", "M1")], 2)
	// A total includes no calculated member: RF + LY only.
	near("sales total", g.Totals[sales], 370)

	// Pinned at a calculated member (a KPI tile): computed from the siblings'
	// totals — (210 - 160) / 160 = 31.25 %, not the months' 20 % + 50 %.
	pinned := func(code string) gridResp {
		b, _ := json.Marshal(map[string]string{scen: code})
		return read("&totals_only=1&scope=" + url.QueryEscape(string(b)))
	}
	near("sales total at VAR", pinned("VAR").Totals[sales], 50)
	near("sales total at VARPCT", pinned("VARPCT").Totals[sales], 31.25)

	// A chart by scenario plots the calculated members too.
	dash := must("POST", "/api/developer/dashboards", map[string]any{"name": "Variance", "revision_id": f.workingRevID})
	chart := must("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{"widget_type": "chart", "ref_id": grid,
		"pos_x": 0, "pos_y": 0, "size_w": 400, "size_h": 300, "widget_props": map[string]any{"chart": map[string]any{
			"chart_type": "bar", "dimension_id": scen, "metric_ids": []string{sales}}}})
	_, raw := call("POST", "/api/dashboard-widgets/"+chart+"/chart-data", map[string]any{"context": map[string]string{}})
	var cd struct {
		Categories []struct {
			Key string `json:"key"`
		} `json:"categories"`
		Series []struct {
			Values []*float64 `json:"values"`
		} `json:"series"`
	}
	_ = json.Unmarshal([]byte(raw), &cd)
	plotted := map[string]float64{}
	for i, c := range cd.Categories {
		if len(cd.Series) > 0 && i < len(cd.Series[0].Values) && cd.Series[0].Values[i] != nil {
			plotted[c.Key] = *cd.Series[0].Values[i]
		}
	}
	near("chart VAR", plotted["VAR"], 50)
	near("chart VARPCT", plotted["VARPCT"], 31.25)

	// ── What the save refuses ──
	a2 := must("POST", members, map[string]any{"code": "A2", "label": "Twice the variance", "formula": "{VAR} * 2"})
	for _, tc := range []struct {
		what, method, path string
		body               any
		want               string
	}{
		{"an unknown member", "PATCH", members + "/" + varID, map[string]any{"code": "VAR", "label": "Variance", "formula": "{RF} - {BUDGET}"}, "not a member"},
		{"itself", "PATCH", members + "/" + varID, map[string]any{"code": "VAR", "label": "Variance", "formula": "{VAR} - {LY}"}, "reads VAR itself"},
		{"a cycle", "PATCH", members + "/" + varID, map[string]any{"code": "VAR", "label": "Variance", "formula": "{A2} - {LY}"}, "circle"},
		{"a function reading metrics", "PATCH", members + "/" + varID, map[string]any{"code": "VAR", "label": "Variance", "formula": "SUM({RF}, {LY})"}, "calls SUM"},
		{"a member under VAR", "POST", members, map[string]any{"code": "SUB", "label": "Sub", "parent_member_id": varID}, "calculated member"},
		{"a LOOKUP of VAR", "POST", "/api/developer/metrics", map[string]any{"name": "bad", "formula": `LOOKUP(sales, scenario, "VAR")`, "revision_id": f.workingRevID}, "calculated member"},
		{"deleting a member VAR reads", "DELETE", members + "/" + ly, nil, "calculated member VAR"},
	} {
		if status, raw := call(tc.method, tc.path, tc.body); status < 400 || !strings.Contains(raw, tc.want) {
			t.Errorf("%s: %d %s, want refused with %q", tc.what, status, raw, tc.want)
		}
	}
	must("DELETE", members+"/"+a2, nil)

	// ── A rename follows into the formulas ──
	var rfID string
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='RF'`, scen).Scan(&rfID)
	must("PATCH", members+"/"+rfID, map[string]any{"code": "FCST", "label": "Rolling forecast"})
	var varFormula string
	_ = f.pool.QueryRow(ctx, `SELECT formula FROM model.dimension_member WHERE id=$1::uuid`, varID).Scan(&varFormula)
	if varFormula != "FCST - {LY}" && varFormula != "{FCST} - {LY}" && !strings.Contains(varFormula, "FCST") {
		t.Errorf("after renaming RF: VAR = %q, want it to read FCST", varFormula)
	}

	// ── Revision copies and export/import carry the formulas ──
	assertCopied := func(where, modelID, revID string) {
		t.Helper()
		var n int
		_ = f.pool.QueryRow(ctx, `
			SELECT count(*) FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
			WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name='scenario' AND m.formula IS NOT NULL`, modelID, revID).Scan(&n)
		if n != 2 {
			t.Errorf("%s: %d calculated members, want 2", where, n)
		}
	}
	revID := must("POST", "/api/developer/revisions", map[string]any{"name": "Calc copy", "source_revision_id": f.workingRevID})
	assertCopied("duplicated revision", f.modelID, revID)
	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("export: %d %v", status, pkg)
	}
	status, res := f.do(t, "POST", "/api/admin/models/import", f.taPersona,
		map[string]any{"application_id": f.appID, "model_name": "Imported calc", "package": pkg})
	if status != http.StatusOK {
		t.Fatalf("import: %d %v", status, res)
	}
	assertCopied("imported model", res["model_id"].(string), res["revision_id"].(string))
}

// waitCalc waits for the calculation of a metric after a write.
func waitCalc(t *testing.T, f *roundTripFixture, metricID string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		var n int
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.calc_result WHERE metric_id=$1::uuid`, metricID).Scan(&n)
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A calculated metric created after the data it reads is calculated when it
// is placed on a grid. It stayed blank until an input changed: found
// rebuilding the P&L by hand on a Scenario dimension (the AI's confirm had
// already been taught to recalculate).
func TestCalcMetricPlacedAfterItsDataIsCalculated(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	status, raw := doAs(t, f, "POST", "/api/cells", dev, f.appID, map[string]any{"model_id": f.modelID, "revision_id": f.workingRevID,
		"metric_id": f.amountMetricID, "dim_codes": map[string]string{f.staffDimID: "STAFF_A1"}, "value": 40})
	if status != http.StatusOK {
		t.Fatalf("write: %d %s", status, raw)
	}
	status, raw = doAs(t, f, "POST", "/api/developer/metrics", dev, f.appID, map[string]any{"name": "double_amount", "formula": "amount * 2", "revision_id": f.workingRevID})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &created)
	if status, raw := doAs(t, f, "POST", "/api/developer/grids/"+f.gridStaffID+"/metrics/"+created.ID, dev, f.appID, nil); status != http.StatusOK {
		t.Fatalf("place: %d %s", status, raw)
	}
	for i := 0; i < 100; i++ {
		var n int
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.calc_result WHERE metric_id=$1::uuid`, created.ID).Scan(&n)
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("a calculated metric placed after its data was never calculated")
}

// A read pinned to one member of a dimension keeps the facts of an input
// that does not have that dimension: such an input is the same at every
// member. The pin dropped them, so a P&L line on a Scenario grid reading a
// prior-year input without a scenario (IF(scenario = "RF", …, rev_ly))
// read 0 at LY — and every KPI tile pinned to LY, and the variance computed
// from it, was wrong.
func TestPinKeepsInputsWithoutThePinnedDimension(t *testing.T) {
	f := setupRollupFixture(t)
	dev := "rollup-test-approver"
	must := func(method, path string, body any) string {
		t.Helper()
		status, raw := doAs(t, f, method, path, dev, f.appID, body)
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		return out.ID
	}
	scen := must("POST", "/api/developer/dimensions", map[string]any{"name": "scenario", "revision_id": f.workingRevID})
	must("POST", "/api/developer/dimensions/"+scen+"/members", map[string]any{"code": "RF", "label": "RF"})
	must("POST", "/api/developer/dimensions/"+scen+"/members", map[string]any{"code": "LY", "label": "LY"})
	must("POST", "/api/developer/dimensions/"+scen+"/members", map[string]any{"code": "VAR", "label": "Var", "formula": "{RF} - {LY}"})
	line := must("POST", "/api/developer/metrics", map[string]any{"name": "line", "formula": `IF(scenario = "RF", 7, amount)`, "revision_id": f.workingRevID})
	grid := must("POST", "/api/developer/grids", map[string]any{"name": "Lines", "revision_id": f.workingRevID})
	must("POST", "/api/developer/grids/"+grid+"/dimensions/"+scen, nil)
	must("POST", "/api/developer/grids/"+grid+"/metrics/"+line, nil)
	// amount has no scenario; 40 recorded.
	must("POST", "/api/cells", map[string]any{"model_id": f.modelID, "revision_id": f.workingRevID, "metric_id": f.amountMetricID,
		"dim_codes": map[string]string{f.staffDimID: "STAFF_A1"}, "value": 40})
	total := func(code string) (float64, bool) {
		b, _ := json.Marshal(map[string]string{scen: code})
		_, raw := doAs(t, f, "GET", "/api/grid?grid_id="+grid+"&totals_only=1&scope="+url.QueryEscape(string(b)), dev, f.appID, nil)
		var g struct {
			Totals map[string]float64 `json:"totals"`
		}
		_ = json.Unmarshal([]byte(raw), &g)
		v, ok := g.Totals[line]
		return v, ok
	}
	var ly float64
	for i := 0; i < 100; i++ {
		if v, ok := total("LY"); ok && v != 0 {
			ly = v
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ly == 0 {
		t.Fatal("line at LY = 0: the pin dropped amount, which has no scenario")
	}
	if v, _ := total("VAR"); v != 7-ly {
		t.Errorf("line at VAR = %v, want RF - LY = %v", v, 7-ly)
	}
}

// A scoped read resolves a formula chain whose evaluation order is the
// reverse of its name order. Its pass loop was bounded by the count still
// unresolved — which shrinks as metrics resolve — so the top of a chain
// deeper than what was left never got a total (operating_margin ←
// operating_profit ← ebitda on a P&L pinned to one scenario).
func TestScopedReadResolvesADeepChain(t *testing.T) {
	f := setupRollupFixture(t)
	dev := "rollup-test-approver"
	place := func(name, formulaText, agg string) string {
		t.Helper()
		status, raw := doAs(t, f, "POST", "/api/developer/metrics", dev, f.appID, map[string]any{"name": name, "formula": formulaText, "agg_rule": agg, "revision_id": f.workingRevID})
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, status, raw)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		if status, raw := doAs(t, f, "POST", "/api/developer/grids/"+f.gridStaffID+"/metrics/"+out.ID, dev, f.appID, nil); status != http.StatusOK {
			t.Fatalf("place %s: %d %s", name, status, raw)
		}
		return out.ID
	}
	place("chain_d", "amount + 1", "sum")
	place("chain_c", "chain_d + 1", "sum")
	place("chain_b", "chain_c + 1", "sum")
	top := place("chain_a", "chain_b * 2", "formula")
	b, _ := json.Marshal(map[string]string{f.staffDimID: "STAFF_A1"})
	for i := 0; i < 100; i++ {
		_, raw := doAs(t, f, "GET", "/api/grid?grid_id="+f.gridStaffID+"&totals_only=1&scope="+url.QueryEscape(string(b)), dev, f.appID, nil)
		var g struct {
			Totals map[string]float64 `json:"totals"`
		}
		_ = json.Unmarshal([]byte(raw), &g)
		if _, ok := g.Totals[top]; ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("chain_a has no total under a pin: its chain resolved one pass too late")
}
