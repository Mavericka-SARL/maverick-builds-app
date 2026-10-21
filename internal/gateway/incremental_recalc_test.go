package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/salesdemo"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// A cell write recalculates only the cells it reaches and writes only what
// changed (calculation.RecalcAffectedCells, region.go). The test that this
// is safe: after cell and batch writes and clears at random cells of the
// sales demo — three hierarchies, a time dimension, LAG and PREVIOUS
// forecasts, formula and ratio totals, plus a LOOKUP, grids with fewer
// dimensions, a text calculation and formulas read at another formula's
// totals added here — every calculated row
// is exactly what a full recalculation then computes. And a cell the write
// cannot reach is not rewritten.
func TestCellWritesRecalculateOnlyWhatTheyReach(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	ctx := context.Background()
	caller := func(method, path string, body any) (map[string]any, error) {
		status, raw := d.req(method, path, d.dev, body)
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("%s %s: status %d: %s", method, path, status, raw)
		}
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		return parsed, nil
	}
	if err := salesdemo.BuildForecast(caller, d.revID, d.model); err != nil {
		t.Fatalf("forecast metrics: %v", err)
	}
	if err := d.model.WriteForecastFacts(caller, d.modelID, d.revID, salesdemo.SampleForecastFacts()); err != nil {
		t.Fatalf("forecast facts: %v", err)
	}
	metric := func(name, formula, agg, format string) string {
		res := d.call("POST", "/api/developer/metrics", d.dev, map[string]any{
			"name": name, "formula": formula, "agg_rule": agg, "format": format, "revision_id": d.revID,
		})
		id, _ := res["id"].(string)
		if id == "" {
			t.Fatalf("create %s: %v", name, res)
		}
		d.metric[name] = id
		return id
	}
	onGrid := func(gridID string, ids ...string) {
		for _, id := range ids {
			d.call("POST", "/api/developer/grids/"+gridID+"/metrics/"+id, d.dev, nil)
		}
	}
	onGrid(d.gridID,
		metric("uk_revenue", `=LOOKUP({revenue}, geography, "UK")`, "sum", "number"),
		metric("margin_status", `=IF({margin} > 0, "profit", "loss")`, "formula", "text"),
		// A floor no cell reaches but totals pass: a write below it changes
		// revenue_floor's totals and none of its cells, which a formula
		// reading it at its totals must still see.
		metric("revenue_floor", "=MAX({revenue}, 1000000)", "formula", "number"),
		metric("revenue_floor_x2", "={revenue_floor} * 2", "formula", "number"),
	)
	regional := d.call("POST", "/api/developer/grids", d.dev, map[string]any{"name": "Regional", "revision_id": d.revID})
	regionalID, _ := regional["id"].(string)
	for _, dimID := range []string{d.geoDim, d.periodDim} {
		d.call("POST", "/api/developer/grids/"+regionalID+"/dimensions/"+dimID, d.dev, nil)
	}
	onGrid(regionalID,
		metric("region_revenue", "={revenue}", "sum", "number"),
		metric("region_margin_pct", "={margin} / {revenue} * 100", "formula", "number"),
		metric("region_floor", "={revenue_floor}", "sum", "number"),
	)
	// One dimension only: read at its countries, revenue_floor answers
	// with its stored geography slices — its formula over each country's
	// total, not its cells.
	countries := d.call("POST", "/api/developer/grids", d.dev, map[string]any{"name": "Countries", "revision_id": d.revID})
	countriesID, _ := countries["id"].(string)
	d.call("POST", "/api/developer/grids/"+countriesID+"/dimensions/"+d.geoDim, d.dev, nil)
	onGrid(countriesID, metric("country_floor", "={revenue_floor}", "sum", "number"))

	sched := calculation.NewScheduler(logger.New("test"), calculation.NewStore(d.pool))
	var calcIDs []string
	if err := d.pool.QueryRow(ctx, `SELECT array_agg(id::text) FROM model.metric_def WHERE revision_id=$1::uuid AND NOT is_input`, d.revID).Scan(&calcIDs); err != nil {
		t.Fatal(err)
	}
	full := func() {
		t.Helper()
		if err := sched.RecalcSpecific(ctx, d.modelID, d.revID, calcIDs); err != nil {
			t.Fatalf("full recalculation: %v", err)
		}
	}
	type row struct {
		value float64
		text  string
		at    time.Time
	}
	snap := func() map[string]row {
		t.Helper()
		rows, err := d.pool.Query(ctx, `
			SELECT DISTINCT ON (metric_id, dim_members) metric_id::text || ' ' || dim_members::text, value::float8, COALESCE(text_value, '<none>'), calc_at
			FROM runtime.calc_result WHERE revision_id = $1::uuid
			ORDER BY metric_id, dim_members, calc_at DESC`, d.revID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]row{}
		for rows.Next() {
			var k string
			var r row
			if err := rows.Scan(&k, &r.value, &r.text, &r.at); err != nil {
				t.Fatal(err)
			}
			out[k] = r
		}
		return out
	}
	names := map[string]string{}
	for n, id := range d.metric {
		names[id] = n
	}
	same := func(label string) {
		t.Helper()
		incremental := snap()
		full()
		whole := snap()
		bad := 0
		for k, w := range whole {
			if g, ok := incremental[k]; !ok || g.value != w.value || g.text != w.text {
				if bad++; bad <= 5 {
					t.Errorf("%s: %s%s incremental %v (present %v), full %v", label, names[k[:36]], k[36:], g, ok, w)
				}
			}
		}
		for k := range incremental {
			if _, ok := whole[k]; !ok {
				if bad++; bad <= 5 {
					t.Errorf("%s: %s%s is a row the full recalculation does not have", label, names[k[:36]], k[36:])
				}
			}
		}
		if bad > 0 {
			t.Fatalf("%s: %d rows differ from a full recalculation", label, bad)
		}
	}
	full()

	// A write the cell (US, LICENSE, Q3) cannot see leaves its margin row
	// as it was: not rewritten.
	key := func(name string, geo, prod, period string) string {
		dm, _ := json.Marshal(map[string]string{d.geoDim: geo, d.prodDim: prod, d.periodDim: period})
		var canon string
		if err := d.pool.QueryRow(ctx, `SELECT $1::jsonb::text`, string(dm)).Scan(&canon); err != nil {
			t.Fatal(err)
		}
		return d.metric[name] + " " + canon
	}
	before := snap()
	if status, raw := d.writeCell(d.dev, "revenue", "UK", "LAPTOP", "Q1", 123456); status != 200 {
		t.Fatalf("write: %d %s", status, raw)
	}
	after := snap()
	if a, b := after[key("margin", "US", "LICENSE", "Q3")], before[key("margin", "US", "LICENSE", "Q3")]; !a.at.Equal(b.at) {
		t.Errorf("margin at US/LICENSE/Q3 was rewritten by a write at UK/LAPTOP/Q1 (%v → %v)", b.at, a.at)
	}
	if a, b := after[key("margin", "UK", "LAPTOP", "Q1")], before[key("margin", "UK", "LAPTOP", "Q1")]; a.value == b.value {
		t.Errorf("margin at UK/LAPTOP/Q1 did not change (%v)", a.value)
	}
	same("one revenue cell")

	// UK monitors in Q1 below the floor, twice: the second write leaves
	// revenue_floor's cell at the floor and moves its total over products
	// in the UK in Q1, which region_floor reads.
	for _, v := range []float64{950000, 990000} {
		if status, raw := d.writeCell(d.dev, "revenue", "UK", "MONITOR", "Q1", v); status != 200 {
			t.Fatalf("write: %d %s", status, raw)
		}
		same(fmt.Sprintf("UK monitors at %v", v))
	}

	geo := []string{"UK", "DE", "US", "CA"}
	prod := []string{"LAPTOP", "MONITOR", "LICENSE", "SUPPORT"}
	period := []string{"Q1", "Q2", "Q3", "Q4"}
	inputs := []string{"units", "revenue", "cost", "target", "seasonality", "pipeline"}
	rng := rand.New(rand.NewSource(7))
	cell := func() map[string]any {
		c := map[string]any{
			"metric_id": d.metric[inputs[rng.Intn(len(inputs))]],
			"dim_codes": map[string]string{d.geoDim: geo[rng.Intn(4)], d.prodDim: prod[rng.Intn(4)], d.periodDim: period[rng.Intn(4)]},
		}
		switch {
		case rng.Intn(8) == 0:
			c["clear"] = true
		case rng.Intn(10) == 0:
			c["value"] = 0
		default:
			c["value"] = float64(rng.Intn(500000)) / 100
		}
		return c
	}
	for round := 0; round < 4; round++ {
		for i := 0; i < 6; i++ {
			body := cell()
			body["model_id"], body["revision_id"] = d.modelID, d.revID
			if status, raw := d.req("POST", "/api/cells", d.dev, body); status != 200 {
				t.Fatalf("write %v: %d %s", body, status, raw)
			}
		}
		var batch []map[string]any
		seen := map[string]bool{}
		for len(batch) < 3 {
			c := cell()
			k := fmt.Sprint(c["metric_id"], c["dim_codes"])
			if !seen[k] {
				seen[k] = true
				batch = append(batch, c)
			}
		}
		if status, raw := d.req("POST", "/api/cells/batch", d.dev, map[string]any{"model_id": d.modelID, "revision_id": d.revID, "cells": batch}); status != 200 {
			t.Fatalf("batch: %d %s", status, raw)
		}
		same(fmt.Sprintf("round %d", round+1))
	}
}
