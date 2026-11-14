package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/readset"
)

// An export with totals holds, at each parent row, the engine's total there
// under the reader's access — what /api/grid answers for that scope — for an
// input metric and for a calculated one served from its stored rows. The
// reads of one export share each served metric's stored values (read once,
// calcValueMemo): the kit's consolidated trial balance pack read them again
// for each of its 228 totals and took 3.3 s; it takes 1 s (2026-11-14).
func TestExportTotalsAreTheEnginesAtEachParent(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	const mirrorFormula = "LOOKUP(revenue, region, region)"
	if !readset.Served(mirrorFormula) {
		t.Fatalf("%s is not served from stored rows; the test needs one that is", mirrorFormula)
	}
	region := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,'region','standard') RETURNING id::text`, f.modelID, f.workingRevID)
	world := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid,'WORLD','World') RETURNING id::text`, region)
	for _, c := range []string{"EU", "US"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid,$2,$2,$3::uuid) RETURNING id::text`, region, c, world)
	}
	revenue := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format) VALUES ($1::uuid,$2::uuid,'revenue',true,'currency') RETURNING id::text`, f.modelID, f.workingRevID)
	mirror := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format, formula) VALUES ($1::uuid,$2::uuid,'mirror',false,'currency',$3) RETURNING id::text`, f.modelID, f.workingRevID, mirrorFormula)
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'Regions') RETURNING id::text`, f.modelID, f.workingRevID)
	q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid,$2::uuid) RETURNING grid_id::text`, grid, region)
	for i, m := range []string{revenue, mirror} {
		q(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid,$2::uuid,$3) RETURNING grid_id::text`, grid, m, i)
	}
	for code, v := range map[string]float64{"EU": 100, "US": 200} {
		cell, _ := json.Marshal(map[string]string{region: code})
		q(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by) VALUES ($1::uuid,$2::uuid,$3::uuid,$4::jsonb,$5,$6::uuid) RETURNING id::text`,
			f.modelID, f.workingRevID, revenue, string(cell), v, f.devID)
	}
	for code, v := range map[string]float64{"EU": 100, "US": 200, "WORLD": 300, "": 300} {
		cell := "{}"
		if code != "" {
			b, _ := json.Marshal(map[string]string{region: code})
			cell = string(b)
		}
		q(`INSERT INTO runtime.calc_result (model_id, revision_id, metric_id, dim_members, value, partition_key) VALUES ($1::uuid,$2::uuid,$3::uuid,$4::jsonb,$5,'p') RETURNING id::text`,
			f.modelID, f.workingRevID, mirror, cell, v)
	}

	status, body := f.do(t, "POST", "/api/developer/integrations?revision_id="+f.workingRevID, f.devSub, map[string]any{
		"type": "file_export", "name": "Regions with totals", "target_type": "grid", "target_id": grid,
		"config": map[string]any{"format": "json", "dimensions": []string{"region"}, "metrics": []string{"revenue", "mirror"}, "include_totals": true},
	})
	if status != http.StatusOK {
		t.Fatalf("create export: %d %s", status, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)

	// The engine's totals at WORLD, as the grid answers them.
	scope, _ := json.Marshal(map[string]string{region: "WORLD"})
	status, body = f.do(t, "GET", "/api/grid?"+url.Values{"grid_def_id": {grid}, "totals_only": {"1"}, "scope": {string(scope)}}.Encode(), f.devSub, nil)
	var engine struct {
		Totals map[string]float64 `json:"totals"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &engine) != nil {
		t.Fatalf("grid totals at WORLD: %d %s", status, body)
	}
	if engine.Totals[revenue] != 300 || engine.Totals[mirror] != 300 {
		t.Fatalf("the engine's totals at WORLD %v; want revenue 300 and mirror 300", engine.Totals)
	}

	// Twice: each download reads its own values once, not the last one's.
	for run := 1; run <= 2; run++ {
		status, body = f.do(t, "GET", "/api/integrations/"+created.ID+"/export", f.devSub, nil)
		if status != http.StatusOK {
			t.Fatalf("download %d: %d %s", run, status, body)
		}
		var rows []map[string]any
		if err := json.Unmarshal(body, &rows); err != nil {
			t.Fatalf("download %d: %v in %s", run, err, body)
		}
		got := map[string]any{}
		for _, r := range rows {
			if r["region"] == "WORLD" {
				got = r
			}
		}
		if got["revenue"] != engine.Totals[revenue] || got["mirror"] != engine.Totals[mirror] {
			t.Fatalf("download %d: WORLD rows %v; want the engine's totals revenue %v, mirror %v (rows %s)",
				run, got, engine.Totals[revenue], engine.Totals[mirror], body)
		}
		if run == 1 {
			// A stored value changed between downloads is read again.
			q(`UPDATE runtime.calc_result SET value = 350 WHERE metric_id = $1::uuid AND dim_members->>$2 = 'WORLD' RETURNING id::text`, mirror, region)
			q(`UPDATE runtime.calc_result SET value = 350 WHERE metric_id = $1::uuid AND dim_members = '{}'::jsonb RETURNING id::text`, mirror)
			status, body = f.do(t, "GET", "/api/grid?"+url.Values{"grid_def_id": {grid}, "totals_only": {"1"}, "scope": {string(scope)}}.Encode(), f.devSub, nil)
			engine.Totals = nil
			if status != http.StatusOK || json.Unmarshal(body, &engine) != nil {
				t.Fatalf("grid totals at WORLD: %d %s", status, body)
			}
		}
	}
}
