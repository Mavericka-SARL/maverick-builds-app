package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A cell write names one member of each dimension of the metric's grid —
// no other dimension, and none missing (OBSERVATIONS "The cell write does not
// check that a dimension is the metric's"). A value written with an extra
// dimension overwrote the real cell in the grid and was counted twice in its
// total; one missing a dimension counted in the total and showed in no cell.
func TestCellWriteRefusesADimensionTheMetricHasNot(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()
	scenario := f.call("POST", "/api/developer/dimensions", map[string]any{"name": "scenario", "revision_id": f.revID, "dimension_type": "standard"})
	for _, code := range []string{"Base", "Alt"} {
		f.call("POST", "/api/developer/dimensions/"+scenario+"/members", map[string]any{"code": code, "label": code})
	}
	f.metric["cost"] = f.call("POST", "/api/developer/metrics", f.metricBody("cost", ""))
	grid2 := f.call("POST", "/api/developer/grids", map[string]any{"name": "Costs", "revision_id": f.revID})
	f.call("POST", "/api/developer/grids/"+grid2+"/dimensions/"+f.region, nil)
	f.call("POST", "/api/developer/grids/"+grid2+"/dimensions/"+scenario, nil)
	f.call("POST", "/api/developer/grids/"+grid2+"/metrics/"+f.metric["cost"], nil)

	cell := func(metric string, codes map[string]string, v float64) map[string]any {
		return map[string]any{"model_id": f.modelID, "metric_id": f.metric[metric], "revision_id": f.revID, "dim_codes": codes, "value": v}
	}
	f.writeCell("revenue", "EMEA", 100)
	f.refused("revenue with a dimension its grid has not", "POST", "/api/cells",
		cell("revenue", map[string]string{f.region: "EMEA", scenario: "Base"}, 999), "NOT_METRIC_DIMENSION")
	f.refused("cost without its scenario", "POST", "/api/cells",
		cell("cost", map[string]string{f.region: "EMEA"}, 70), "MISSING_DIMENSION")
	f.refused("cost without its region", "POST", "/api/cells",
		cell("cost", map[string]string{scenario: "Alt"}, 30), "MISSING_DIMENSION")
	f.call("POST", "/api/cells", cell("cost", map[string]string{f.region: "EMEA", scenario: "Base"}, 5))

	// A batch with one such cell is refused at that cell, and writes nothing.
	status, raw := f.req("POST", "/api/cells/batch", map[string]any{
		"model_id": f.modelID, "revision_id": f.revID,
		"cells": []map[string]any{
			{"metric_id": f.metric["cost"], "dim_codes": map[string]string{f.region: "US", scenario: "Base"}, "value": 1},
			{"metric_id": f.metric["cost"], "dim_codes": map[string]string{f.region: "US"}, "value": 2},
		},
	})
	if status == http.StatusOK || !strings.Contains(string(raw), "MISSING_DIMENSION") {
		t.Errorf("batch with a cell missing a dimension: %d %s; want it refused with MISSING_DIMENSION", status, raw)
	} else {
		var out struct {
			Refused []struct {
				Index int `json:"index"`
			} `json:"refused"`
		}
		_ = json.Unmarshal(raw, &out)
		if len(out.Refused) != 1 || out.Refused[0].Index != 1 {
			t.Errorf("batch refusal = %s, want one at index 1", raw)
		}
	}

	var facts int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, f.modelID).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 2 {
		t.Errorf("fact rows = %d, want 2 (revenue EMEA, cost EMEA:Base): a refused write stored something", facts)
	}

	// A value stored before the check with an extra dimension can still be
	// cleared by naming its exact coordinate.
	stray := fmt.Sprintf(`{"%s":"US","%s":"Alt"}`, f.region, scenario)
	if _, err := f.pool.Exec(ctx, `INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, 7)`, f.modelID, f.revID, f.metric["revenue"], stray); err != nil {
		t.Fatal(err)
	}
	f.call("POST", "/api/cells", map[string]any{"model_id": f.modelID, "metric_id": f.metric["revenue"], "revision_id": f.revID,
		"dim_codes": map[string]string{f.region: "US", scenario: "Alt"}, "clear": true})
	var left int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE metric_id=$1::uuid AND dim_members=$2::jsonb`, f.metric["revenue"], stray).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("the stray coordinate is still stored after a clear naming it (%d rows)", left)
	}
}

// Copying a member's values skips a value stored at a coordinate its metric
// does not have (written before the check), instead of copying it into
// another stray coordinate or refusing the whole copy.
func TestCopyValuesSkipsAStoredCoordinateTheMetricHasNot(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	ctx := context.Background()
	// UK laptop revenue with no period: no grid of revenue reads it.
	if _, err := d.pool.Exec(ctx, `INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, 1)`, d.modelID, d.revID, d.metric["revenue"],
		fmt.Sprintf(`{"%s":"UK","%s":"LAPTOP"}`, d.geoDim, d.prodDim)); err != nil {
		t.Fatal(err)
	}
	dry := d.call("POST", "/api/business-admin/copy-values", d.admin,
		map[string]any{"dimension_id": d.geoDim, "from": "UK", "to": "US", "dry_run": true})
	if dry["cells"] != float64(4) {
		t.Errorf("dry run with a stray UK value: %v, want the UK's 4 values (the stray skipped)", dry)
	}
}
