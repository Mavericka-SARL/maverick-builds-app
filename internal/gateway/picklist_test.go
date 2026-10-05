package gateway

// Pick-lists: a metric whose cells hold members of a dimension (migration
// 110) — a Status of Draft / Cancelled, an activity's Region — as a form's
// dimension field does. Driven through the developer API and /api/cells as
// a strategic-activities sheet: per activity a Region and a Status, an
// amount; per region the amount of its activities that are not cancelled
// (SUMIFS over the pick-lists) and a calculated status.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPicklistMetrics(t *testing.T) {
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
	dim := func(name string, codes ...string) (string, map[string]string) {
		id := must("POST", "/api/developer/dimensions", map[string]any{"name": name, "revision_id": rev})
		ids := map[string]string{}
		for _, c := range codes {
			ids[c] = must("POST", "/api/developer/dimensions/"+id+"/members", map[string]any{"code": c, "label": c + " label"})
		}
		return id, ids
	}
	statuses, statusMembers := dim("activity_statuses", "Draft", "Cancelled")
	activity, _ := dim("activity", "A1", "A2", "A3")
	region, _ := dim("sales_region", "NA", "EU")
	metric := func(body map[string]any) string {
		body["revision_id"] = rev
		return must("POST", "/api/developer/metrics", body)
	}
	actStatus := metric(map[string]any{"name": "act_status", "is_input": true, "format": "picklist", "picklist_dimension_id": "activity_statuses"})
	actRegion := metric(map[string]any{"name": "act_region", "is_input": true, "format": "picklist", "picklist_dimension_id": region})
	strat := metric(map[string]any{"name": "strat", "is_input": true, "format": "currency"})
	gAct := must("POST", "/api/developer/grids", map[string]any{"name": "Activities", "revision_id": rev})
	for _, p := range []string{"/dimensions/" + activity, "/metrics/" + actStatus, "/metrics/" + actRegion, "/metrics/" + strat} {
		must("POST", "/api/developer/grids/"+gAct+p, nil)
	}
	gReg := must("POST", "/api/developer/grids", map[string]any{"name": "Regions", "revision_id": rev})
	must("POST", "/api/developer/grids/"+gReg+"/dimensions/"+region, nil)
	byRegion := metric(map[string]any{"name": "by_region", "formula": `SUMIFS(strat, act_region, sales_region, act_status, "<>Cancelled")`})
	regStatus := metric(map[string]any{"name": "region_status", "format": "picklist", "picklist_dimension_id": statuses, "agg_rule": "formula",
		"formula": `IF(by_region > 15, "Cancelled", "Draft")`})
	for _, m := range []string{byRegion, regStatus} {
		must("POST", "/api/developer/grids/"+gReg+"/metrics/"+m, nil)
	}

	write := func(metricID, act string, body map[string]any) (int, string) {
		body["model_id"], body["revision_id"], body["metric_id"] = f.modelID, rev, metricID
		body["dim_codes"] = map[string]string{activity: act}
		return call("POST", "/api/cells", body)
	}
	for _, w := range []struct {
		act, reg, status string
		amount           float64
	}{{"A1", "NA", "Draft", 10}, {"A2", "NA", "Cancelled", 5}, {"A3", "EU label", "draft", 7}} { // by code, by label, any case
		for m, v := range map[string]string{actRegion: w.reg, actStatus: w.status} {
			if status, raw := write(m, w.act, map[string]any{"member": v}); status != http.StatusOK {
				t.Fatalf("write %s %s: %d %s", w.act, v, status, raw)
			}
		}
		if status, raw := write(strat, w.act, map[string]any{"value": w.amount}); status != http.StatusOK {
			t.Fatalf("write amount: %d %s", status, raw)
		}
	}
	// A pick-list cell holds a member of its dimension, nothing else.
	if status, raw := write(actStatus, "A1", map[string]any{"member": "Approved"}); status != http.StatusBadRequest || !strings.Contains(raw, "no member") {
		t.Errorf("an unknown member: %d %s, want 400", status, raw)
	}
	if status, raw := write(actStatus, "A1", map[string]any{"value": 3}); status != http.StatusBadRequest {
		t.Errorf("a number that is no member's key: %d %s, want 400", status, raw)
	}

	type gridResp struct {
		Metrics []struct {
			ID      string `json:"id"`
			Options []struct {
				Key   float64 `json:"key"`
				Code  string  `json:"code"`
				Label string  `json:"label"`
			} `json:"picklist_options"`
		} `json:"metrics"`
		Cells  map[string]float64 `json:"cells"`
		Totals map[string]float64 `json:"totals"`
	}
	read := func(grid string) gridResp {
		t.Helper()
		status, raw := call("GET", "/api/grid?grid_def_id="+grid+"&model_id="+f.modelID+"&revision_id="+rev, nil)
		if status != http.StatusOK {
			t.Fatalf("grid: %d %s", status, raw)
		}
		var g gridResp
		_ = json.Unmarshal([]byte(raw), &g)
		return g
	}
	label := func(g gridResp, metricID string, key float64) string {
		for _, m := range g.Metrics {
			if m.ID == metricID {
				for _, o := range m.Options {
					if o.Key == key {
						return o.Label
					}
				}
			}
		}
		return "?"
	}

	// The cells show their members; a pick-list input has no total.
	g := read(gAct)
	if l := label(g, actStatus, g.Cells[actStatus+":A2"]); l != "Cancelled label" {
		t.Errorf("A2's status shows %q, want \"Cancelled label\"", l)
	}
	if l := label(g, actRegion, g.Cells[actRegion+":A3"]); l != "EU label" {
		t.Errorf("A3's region shows %q, want \"EU label\"", l)
	}
	if v, ok := g.Totals[actStatus]; ok {
		t.Errorf("a pick-list input has a total %v, want none", v)
	}
	// SUMIFS over the pick-lists, and a calculated pick-list.
	r := read(gReg)
	if r.Cells[byRegion+":NA"] != 10 || r.Cells[byRegion+":EU"] != 7 {
		t.Errorf("by_region NA=%v EU=%v, want 10 (A2 cancelled) and 7", r.Cells[byRegion+":NA"], r.Cells[byRegion+":EU"])
	}
	if l := label(r, regStatus, r.Cells[regStatus+":NA"]); l != "Draft label" {
		t.Errorf("NA's status %q, want Draft", l)
	}
	// Un-cancelling A2 recalculates both (the pick-list range is a dependency).
	if status, raw := write(actStatus, "A2", map[string]any{"member": "Draft"}); status != http.StatusOK {
		t.Fatalf("re-draft A2: %d %s", status, raw)
	}
	r = read(gReg)
	if r.Cells[byRegion+":NA"] != 15 {
		t.Errorf("by_region NA after A2 drafted = %v, want 15", r.Cells[byRegion+":NA"])
	}
	if l := label(r, regStatus, r.Totals[regStatus]); l != "Cancelled label" {
		t.Errorf("the status at the total (22 > 15) = %v (%s), want Cancelled: agg_rule formula evaluates it there", r.Totals[regStatus], l)
	}

	// Refusals.
	refused := func(what string, status int, raw string, want int, text string) {
		t.Helper()
		if status != want || !strings.Contains(raw, text) {
			t.Errorf("%s: %d %s, want %d mentioning %q", what, status, raw, want, text)
		}
	}
	st, raw := call("POST", "/api/developer/metrics", map[string]any{"name": "bad_sum", "is_input": true, "format": "picklist",
		"picklist_dimension_id": statuses, "agg_rule": "sum", "revision_id": rev})
	refused("a pick-list that sums", st, raw, http.StatusBadRequest, "never added up")
	st, raw = call("POST", "/api/developer/metrics", map[string]any{"name": "no_dim", "is_input": true, "format": "picklist", "revision_id": rev})
	refused("a pick-list without a dimension", st, raw, http.StatusBadRequest, "picklist_dimension")
	st, raw = call("POST", "/api/developer/metrics", map[string]any{"name": "activity_statuses", "is_input": true, "revision_id": rev})
	refused("a metric named like a dimension", st, raw, http.StatusConflict, "already the name of a dimension")
	st, raw = call("POST", "/api/developer/dimensions", map[string]any{"name": "act_status", "revision_id": rev})
	refused("a dimension named like a metric", st, raw, http.StatusConflict, "already the name of a metric")
	// A criterion naming a parent member matches no leaf: refused, with the
	// reads that do mean its total.
	period := must("POST", "/api/developer/dimensions", map[string]any{"name": "period_x", "revision_id": rev})
	fy := must("POST", "/api/developer/dimensions/"+period+"/members", map[string]any{"code": "FY", "label": "FY"})
	must("POST", "/api/developer/dimensions/"+period+"/members", map[string]any{"code": "M1", "label": "M1", "parent_member_id": fy})
	st, raw = call("POST", "/api/developer/metrics", map[string]any{"name": "fy_strat", "formula": `SUMIFS(strat, period_x, "FY")`, "revision_id": rev})
	refused("a SUMIFS criterion naming a parent", st, raw, http.StatusBadRequest, "matches nothing")
	st, raw = call("DELETE", "/api/developer/dimensions/"+statuses, nil)
	refused("deleting a dimension pick-lists hold", st, raw, http.StatusConflict, "act_status")
	if status, raw := write(actStatus, "A2", map[string]any{"member": "Cancelled"}); status != http.StatusOK {
		t.Fatalf("cancel A2: %d %s", status, raw)
	}
	st, raw = call("DELETE", "/api/developer/dimensions/"+statuses+"/members/"+statusMembers["Cancelled"], nil)
	refused("deleting a member pick-list cells hold", st, raw, http.StatusConflict, "act_status")
	dash := must("POST", "/api/developer/dashboards", map[string]any{"name": "Picklist charts"})
	st, raw = call("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{"widget_type": "chart", "ref_id": gAct,
		"pos_x": 0, "pos_y": 0, "size_w": 400, "size_h": 300,
		"widget_props": map[string]any{"chart": map[string]any{"chart_type": "bar", "dimension_id": activity, "metric_ids": []string{actStatus}}}})
	refused("a chart of a pick-list", st, raw, http.StatusBadRequest, "pick-list")
	st, raw = call("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{"widget_type": "workflow_button",
		"pos_x": 0, "pos_y": 400, "size_w": 200, "size_h": 40})
	refused("a widget type the console does not draw", st, raw, http.StatusBadRequest, "automation_button")

	// Renaming a member's code keeps the cells holding it.
	must("PATCH", "/api/developer/dimensions/"+statuses+"/members/"+statusMembers["Cancelled"], map[string]any{"code": "Dropped", "label": "Dropped label"})
	g = read(gAct)
	if l := label(g, actStatus, g.Cells[actStatus+":A2"]); l != "Dropped label" {
		t.Errorf("after the rename A2 shows %q, want \"Dropped label\"", l)
	}

	// A revision copy's pick-lists hold members of the copy's own dimension.
	copyRev := must("POST", "/api/developer/revisions", map[string]any{"name": "Picklist copy", "source_revision_id": rev})
	var ownDim bool
	if err := f.pool.QueryRow(ctx, `
		SELECT d.revision_id = m.revision_id AND d.name = 'activity_statuses'
		FROM model.metric_def m JOIN model.dimension_def d ON d.id = m.picklist_dimension_id
		WHERE m.revision_id = $1::uuid AND m.name = 'act_status'`, copyRev).Scan(&ownDim); err != nil || !ownDim {
		t.Errorf("the copy's act_status: own dimension %v (%v), want the copy's activity_statuses", ownDim, err)
	}
}
