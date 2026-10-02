package gateway

// Business users read and work in the model's active revision only; a
// revision being built is its builders' (revision_access.go). Each route
// that takes a revision — by parameter, in its body, or through an object
// that lives in one — is asked for a draft as a business user and as the
// developer.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

func TestRevisionsBeyondTheActiveOneAreTheBuilders(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	ctx := context.Background()

	draft := idOf(d.call("POST", "/api/developer/revisions", d.dev, map[string]any{"name": "Next year", "source_revision_id": d.revID}))
	var draftGrid string
	if err := d.pool.QueryRow(ctx, `SELECT id::text FROM model.grid_def WHERE revision_id=$1::uuid`, draft).Scan(&draftGrid); err != nil {
		t.Fatalf("the draft has no grid of its own: %v", err)
	}
	// Renamed in the draft, so the active revision has no counterpart a read
	// by id could be redirected to.
	if _, err := d.pool.Exec(ctx, `UPDATE model.grid_def SET name='Next year plan' WHERE id=$1::uuid`, draftGrid); err != nil {
		t.Fatal(err)
	}
	// The draft's own copy of the geography dimension.
	var draftGeo string
	if err := d.pool.QueryRow(ctx, `SELECT d.id::text FROM model.grid_dimension gd JOIN model.dimension_def d ON d.id = gd.dimension_id
		WHERE gd.grid_id=$1::uuid AND d.name='geography'`, draftGrid).Scan(&draftGeo); err != nil {
		t.Fatal(err)
	}
	var draftMetric string
	_ = d.pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE revision_id=$1::uuid AND name='revenue'`, draft).Scan(&draftMetric)
	// A dashboard, a chart and a form that exist only in the draft.
	dash := idOf(d.call("POST", "/api/developer/dashboards", d.dev, map[string]any{"name": "Draft board", "revision_id": draft}))
	chart := idOf(d.call("POST", "/api/developer/dashboards/"+dash+"/widgets", d.dev, map[string]any{
		"widget_type": "chart", "ref_id": draftGrid, "size_w": 400, "size_h": 300,
		"widget_props": map[string]any{"chart": map[string]any{"chart_type": "bar", "dimension_id": draftGeo,
			"metric_ids": []string{draftMetric}, "context_defaults": map[string]string{}}},
	}))
	form := idOf(d.call("POST", "/api/forms?revision_id="+draft, d.dev, map[string]any{
		"name": "plan_note", "label": "Plan note", "fields": []map[string]any{{"name": "note", "label": "Note", "type": "text"}},
	}))

	q := func(v url.Values) string { return "?" + v.Encode() }
	draftQ := url.Values{"revision_id": {draft}}
	cases := []struct {
		name, method, path string
		body               any
	}{
		{"grid", "GET", "/api/grid" + q(url.Values{"revision_id": {draft}, "grid_def_id": {draftGrid}}), nil},
		{"grid of the draft named by id alone", "GET", "/api/grid" + q(url.Values{"grid_def_id": {draftGrid}, "meta_only": {"1"}}), nil},
		{"grid catalog", "GET", "/api/grids" + q(draftQ), nil},
		{"grid series", "GET", "/api/grid/series" + q(url.Values{"revision_id": {draft}, "grid_def_id": {draftGrid},
			"dimension_id": {draftGeo}, "metric_ids": {draftMetric}}), nil},
		{"grid export", "GET", "/api/grid/export" + q(url.Values{"revision_id": {draft}, "grid_def_id": {draftGrid}}), nil},
		{"metrics", "GET", "/api/metrics" + q(draftQ), nil},
		{"dimensions", "GET", "/api/dimensions" + q(draftQ), nil},
		{"forms", "GET", "/api/forms" + q(draftQ), nil},
		{"a draft form's records", "GET", "/api/forms/" + form + "/records", nil},
		{"folders", "GET", "/api/folders" + q(draftQ), nil},
		{"a draft dashboard", "GET", "/api/dashboards/" + dash, nil},
		{"a draft chart's data", "POST", "/api/dashboard-widgets/" + chart + "/chart-data", map[string]any{"context": map[string]string{}}},
		{"automation rules", "GET", "/api/automation/rules" + q(draftQ), nil},
		{"a write into the draft", "POST", "/api/cells", map[string]any{
			"model_id": d.modelID, "revision_id": draft, "metric_id": draftMetric,
			"dim_codes": map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP", d.periodDim: "Q1"}, "value": 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := d.req(c.method, c.path, d.westRep, c.body)
			if status != http.StatusNotFound && status != http.StatusForbidden {
				t.Errorf("business user: %d %s, want the draft refused", status, raw)
			}
			if status, raw := d.req(c.method, c.path, d.dev, c.body); status >= 400 {
				t.Errorf("developer: %d %s, want the draft open to its builder", status, raw)
			}
		})
	}

	t.Run("the active revision stays open, named or not", func(t *testing.T) {
		for _, path := range []string{"/api/grid?grid_def_id=" + d.gridID, "/api/grid?grid_def_id=" + d.gridID + "&revision_id=" + d.revID, "/api/grids?revision_id=" + d.revID} {
			if status, raw := d.req("GET", path, d.westRep, nil); status != http.StatusOK {
				t.Errorf("%s: %d %s", path, status, raw)
			}
		}
	})

	t.Run("a draft refusal reads like a revision that does not exist", func(t *testing.T) {
		_, draftRaw := d.req("GET", "/api/grids?revision_id="+draft, d.westRep, nil)
		_, noneRaw := d.req("GET", "/api/grids?revision_id=00000000-0000-0000-0000-000000000000", d.westRep, nil)
		var a, b map[string]any
		_ = json.Unmarshal(draftRaw, &a)
		_ = json.Unmarshal(noneRaw, &b)
		if a["error"] != b["error"] {
			t.Errorf("draft %v vs unknown %v", a, b)
		}
	})
}
