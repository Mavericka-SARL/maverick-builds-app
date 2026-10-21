package gateway

// A served metric (one that LOOKUPs, so its values are the scheduler's rows)
// with the Formula rule had no total under two pins or more: an education
// model's revenue variance % — LOOKUP of revenue at the Actual and Plan
// views, on a [segment, month, scenario] grid — showed an empty FY column and
// an empty KPI tile with month and scenario pinned, though every cell was
// there. Driven through the developer API.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestServedFormulaTotalUnderTwoPins(t *testing.T) {
	f := setupRoundTripFixture(t)
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
	dim := func(name string, members ...[2]string) string {
		id := must("POST", "/api/developer/dimensions", map[string]any{"name": name, "revision_id": f.workingRevID})
		ids := map[string]string{}
		for _, m := range members {
			body := map[string]any{"code": m[0], "label": m[0]}
			if m[1] != "" {
				body["parent_member_id"] = ids[m[1]]
			}
			ids[m[0]] = must("POST", "/api/developer/dimensions/"+id+"/members", body)
		}
		return id
	}
	seg := dim("segment", [2]string{"ALL", ""}, [2]string{"A", "ALL"}, [2]string{"B", "ALL"})
	view := dim("view", [2]string{"ACT", ""}, [2]string{"PLAN", ""})
	scen := dim("scen", [2]string{"S1", ""}, [2]string{"S2", ""})
	per := dim("per", [2]string{"X", ""}, [2]string{"Y", ""})

	amt := must("POST", "/api/developer/metrics", map[string]any{"name": "amt", "is_input": true, "revision_id": f.workingRevID})
	src := must("POST", "/api/developer/grids", map[string]any{"name": "Source", "revision_id": f.workingRevID})
	for _, p := range []string{"/metrics/" + amt, "/dimensions/" + seg, "/dimensions/" + view, "/dimensions/" + scen, "/dimensions/" + per} {
		must("POST", "/api/developer/grids/"+src+p, nil)
	}
	pct := must("POST", "/api/developer/metrics", map[string]any{"name": "act_vs_plan_pct", "agg_rule": "formula", "format": "percentage",
		"formula": `IFERROR(LOOKUP(amt, view, "ACT") / LOOKUP(amt, view, "PLAN") * 100, 0)`, "revision_id": f.workingRevID})
	ratios := must("POST", "/api/developer/grids", map[string]any{"name": "Ratios", "revision_id": f.workingRevID})
	for _, p := range []string{"/dimensions/" + seg, "/dimensions/" + scen, "/dimensions/" + per, "/metrics/" + pct} {
		must("POST", "/api/developer/grids/"+ratios+p, nil)
	}
	for _, w := range []struct {
		seg, view string
		v         float64
	}{{"A", "ACT", 30}, {"A", "PLAN", 60}, {"B", "ACT", 10}, {"B", "PLAN", 40}} {
		must("POST", "/api/cells", map[string]any{"model_id": f.modelID, "revision_id": f.workingRevID, "metric_id": amt,
			"dim_codes": map[string]string{seg: w.seg, view: w.view, scen: "S1", per: "X"}, "value": w.v})
	}

	type gridResp struct {
		Cells  map[string]float64 `json:"cells"`
		Totals map[string]float64 `json:"totals"`
	}
	read := func(scope map[string]string) gridResp {
		t.Helper()
		raw, _ := json.Marshal(scope)
		q := fmt.Sprintf("/api/grid?grid_id=%s&revision_id=%s&totals_only=1&scope=%s", ratios, f.workingRevID, string(raw))
		var g gridResp
		_, body := call("GET", q, nil)
		_ = json.Unmarshal([]byte(body), &g)
		return g
	}
	// The scheduler's rows land after the writes: wait for the leaf.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, ok := read(map[string]string{seg: "A", scen: "S1", per: "X"}).Totals[pct]; ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	got, ok := read(map[string]string{scen: "S1", per: "X"}).Totals[pct]
	if !ok {
		t.Fatal("no total for the served formula metric under the scen and per pins")
	}
	if want := 40.0 / 100 * 100; math.Abs(got-want) > 1e-9 {
		t.Errorf("total = %v, want %v (the formula at ALL: 40 / 100)", got, want)
	}
	// A dimension with no single root has no total to file: none is made up.
	if v, ok := read(map[string]string{seg: "A", per: "X"}).Totals[pct]; ok {
		t.Errorf("total across scenarios = %v; a formula total over two roots must stay empty", v)
	}
}
