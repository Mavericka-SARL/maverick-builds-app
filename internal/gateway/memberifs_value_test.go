package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
)

// MEMBERIFS, the reverse of LOOKUP, and VALUE through the calculation: a
// territory's owner is the seller whose territory is this one (the
// workbook's HRIS roster names each seller's territory), and a number is
// read from a text metric ("FY 2027"). Neither could be written before:
// the territory owner had to be a pick-list on the territory, and the year
// a second, numeric input (Territory & Quota build, 2026-11-16).
func TestMemberIfsAndValueCalculate(t *testing.T) {
	f := setupRoundTripFixture(t)
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
	dim := func(name string, codes ...string) string {
		id := must("POST", "/api/developer/dimensions", map[string]any{"name": name, "revision_id": rev})
		for _, c := range codes {
			must("POST", "/api/developer/dimensions/"+id+"/members", map[string]any{"code": c, "label": c})
		}
		return id
	}
	rep, terr := dim("seller", "R1", "R2", "R3"), dim("terr", "T1", "T2", "T3")
	metric := func(body map[string]any) string {
		body["revision_id"] = rev
		return must("POST", "/api/developer/metrics", body)
	}
	grid := func(name string, dims []string, metrics ...string) string {
		g := must("POST", "/api/developer/grids", map[string]any{"name": name, "revision_id": rev})
		for _, d := range dims {
			must("POST", "/api/developer/grids/"+g+"/dimensions/"+d, nil)
		}
		for _, m := range metrics {
			must("POST", "/api/developer/grids/"+g+"/metrics/"+m, nil)
		}
		return g
	}
	repTerr := metric(map[string]any{"name": "rep_terr", "is_input": true, "format": "picklist", "picklist_dimension_id": terr})
	grid("Sellers", []string{rep}, repTerr)
	owner := metric(map[string]any{"name": "terr_owner", "format": "picklist", "picklist_dimension_id": rep, "agg_rule": "none",
		"formula": "IFNA(MEMBERIFS(rep_terr, terr), \"\")"})
	gTerr := grid("Territories", []string{terr}, owner)
	planYear := metric(map[string]any{"name": "plan_year", "is_input": true, "format": "text"})
	nextYear := metric(map[string]any{"name": "next_year", "formula": "VALUE(RIGHT(plan_year, 4)) + 1"})
	gSet := grid("Settings", nil, planYear, nextYear)

	write := func(metricID string, dims map[string]string, body map[string]any) {
		t.Helper()
		body["model_id"], body["revision_id"], body["metric_id"], body["dim_codes"] = f.modelID, rev, metricID, dims
		if status, raw := call("POST", "/api/cells", body); status != http.StatusOK {
			t.Fatalf("write %v: %d %s", body, status, raw)
		}
	}
	write(repTerr, map[string]string{rep: "R1"}, map[string]any{"member": "T2"})
	write(repTerr, map[string]string{rep: "R2"}, map[string]any{"member": "T1"})
	write(planYear, map[string]string{}, map[string]any{"text": "FY 2027"})

	type gridResp struct {
		Metrics []struct {
			ID      string `json:"id"`
			Options []struct {
				Key  float64 `json:"key"`
				Code string  `json:"code"`
			} `json:"picklist_options"`
		} `json:"metrics"`
		Cells  map[string]float64 `json:"cells"`
		Totals map[string]float64 `json:"totals"`
	}
	read := func(g string) gridResp {
		t.Helper()
		status, raw := call("GET", "/api/grid?grid_def_id="+g+"&model_id="+f.modelID+"&revision_id="+rev, nil)
		if status != http.StatusOK {
			t.Fatalf("grid: %d %s", status, raw)
		}
		var out gridResp
		_ = json.Unmarshal([]byte(raw), &out)
		return out
	}
	g := read(gTerr)
	code := func(key float64, ok bool) string {
		if !ok {
			return "(none)"
		}
		for _, m := range g.Metrics {
			if m.ID == owner {
				for _, o := range m.Options {
					if o.Key == key {
						return o.Code
					}
				}
			}
		}
		return "?"
	}
	for terrCode, want := range map[string]string{"T1": "R2", "T2": "R1", "T3": "(none)"} {
		v, ok := g.Cells[owner+":"+terrCode]
		if ok && v == 0 {
			ok = false
		}
		if got := code(v, ok); got != want {
			t.Errorf("owner of %s = %s, want %s", terrCode, got, want)
		}
	}
	if s := read(gSet); s.Totals[nextYear] != 2028 {
		t.Errorf("VALUE(RIGHT(plan_year, 4)) + 1 = %v, want 2028", s.Totals[nextYear])
	}
}
