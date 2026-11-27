package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// A record saved in a table that keeps a dimension changes its member's
// properties, and the pass after it follows that member
// (calculation.RecalcMemberProperties, incremental.go): the metrics reading
// the property at their own member are evaluated at that member only — a
// time-series one too, past the lean threshold or not — and what they
// change is followed through a SUMIFS over the members and a formula after
// it. The test that this is safe: after property changes at random members,
// every calculated row is exactly what a full recalculation computes; and
// another member's rows are not rewritten.
func TestMemberPropertyChangesRecalculateOnlyWhatTheyReach(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	lineDim := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "cost_line", "revision_id": d.revID}))
	for _, p := range []struct{ name, typ string }{{"quantity", "number"}, {"rate", "number"}, {"geo", "text"}} {
		d.call("POST", "/api/developer/dimensions/"+lineDim+"/properties", d.dev, map[string]any{"name": p.name, "data_type": p.typ})
	}
	table := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "cost_lines", "label": "Cost lines", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "code", "label": "Line", "type": "text", "code": true, "core": true},
			{"name": "name", "label": "Name", "type": "text", "required": true},
			{"name": "quantity", "label": "Quantity", "type": "number"},
			{"name": "rate", "label": "Rate", "type": "number"},
			{"name": "geo", "label": "Country", "type": "text"},
		},
		"keeps_dimension_id": lineDim, "member_label_field": "name",
	}))
	geos := []string{"UK", "DE", "US", "CA"}
	rng := rand.New(rand.NewSource(7))
	recs := map[string]string{}
	data := map[string]map[string]any{}
	for i := 1; i <= 12; i++ {
		code := fmt.Sprintf("CL%02d", i)
		data[code] = map[string]any{"code": code, "name": "Line " + code, "quantity": float64(1 + rng.Intn(50)), "rate": float64(10 + rng.Intn(90)), "geo": geos[rng.Intn(len(geos))]}
		recs[code] = idOf(d.call("POST", "/api/forms/"+table+"/records", d.dev, map[string]any{"data": data[code]}))
	}
	metric := func(name, formula string) string {
		body := map[string]any{"name": name, "formula": formula, "agg_rule": "sum", "format": "number", "revision_id": d.revID}
		st, raw := d.req("POST", "/api/developer/metrics", d.dev, body)
		if st != 200 && st != 201 {
			t.Fatalf("create %s: %d %s", name, st, raw)
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out["id"].(string)
	}
	grid := func(name string, dims ...string) string {
		g := idOf(d.call("POST", "/api/developer/grids", d.dev, map[string]any{"name": name, "revision_id": d.revID}))
		for _, dim := range dims {
			d.call("POST", "/api/developer/grids/"+g+"/dimensions/"+dim, d.dev, nil)
		}
		return g
	}
	lines := grid("Cost lines by quarter", lineDim, d.periodDim)
	byGeo := grid("By country", d.geoDim, d.periodDim)
	ids := map[string]string{}
	for _, m := range []struct{ grid, name, formula string }{
		{lines, "cl_amt", "cost_line.quantity * cost_line.rate"},
		{lines, "clf", "cl_amt + LAG(cl_amt, 1, 0)"}, // the time-series path
		{byGeo, "geo_lines", "SUMIFS(clf, cost_line.geo, geography)"},
		{byGeo, "geo_double", "geo_lines * 2"},
	} {
		ids[m.name] = metric(m.name, m.formula)
		d.call("POST", "/api/developer/grids/"+m.grid+"/metrics/"+ids[m.name], d.dev, nil)
	}
	names := map[string]string{}
	for n, id := range ids {
		names[id] = n
	}

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
		at    time.Time
	}
	snap := func() map[string]row {
		t.Helper()
		rows, err := d.pool.Query(ctx, `
			SELECT DISTINCT ON (metric_id, dim_members) metric_id::text || ' ' || dim_members::text, value::float8, calc_at
			FROM runtime.calc_result WHERE revision_id = $1::uuid AND metric_id::text = ANY($2::text[])
			ORDER BY metric_id, dim_members, calc_at DESC`, d.revID, []string{ids["cl_amt"], ids["clf"], ids["geo_lines"], ids["geo_double"]})
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]row{}
		for rows.Next() {
			var k string
			var r row
			if err := rows.Scan(&k, &r.value, &r.at); err != nil {
				t.Fatal(err)
			}
			out[k] = r
		}
		return out
	}
	same := func(label string) {
		t.Helper()
		incremental := snap()
		full()
		whole := snap()
		bad := 0
		for k, w := range whole {
			if g, ok := incremental[k]; !ok || g.value != w.value {
				if bad++; bad <= 5 {
					t.Errorf("%s: %s%s incremental %v (present %v), full %v", label, names[k[:36]], k[36:], g.value, ok, w.value)
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
	change := func(code string, patch map[string]any) {
		t.Helper()
		for k, v := range patch {
			data[code][k] = v
		}
		if st, raw := d.req("PUT", "/api/records/"+recs[code], d.dev, map[string]any{"data": data[code]}); st != 200 {
			t.Fatalf("save %s: %d %s", code, st, raw)
		}
		// The save recalculates in the background; this pass follows it on
		// the revision's queue.
		if err := sched.RecalcMemberProperties(ctx, lineDim, []string{code}); err != nil {
			t.Fatalf("recalculate after %s: %v", code, err)
		}
	}
	key := func(metricName, line, period string) string {
		dm, _ := json.Marshal(map[string]string{lineDim: line, d.periodDim: period})
		var canon string
		if err := d.pool.QueryRow(ctx, `SELECT $1::jsonb::text`, string(dm)).Scan(&canon); err != nil {
			t.Fatal(err)
		}
		return ids[metricName] + " " + canon
	}
	full()

	// One line's quantity: its phasing moves, another line's is not rewritten.
	before := snap()
	change("CL01", map[string]any{"quantity": data["CL01"]["quantity"].(float64) + 7})
	after := snap()
	if a, b := after[key("clf", "CL05", "Q3")], before[key("clf", "CL05", "Q3")]; !a.at.Equal(b.at) {
		t.Errorf("clf at CL05/Q3 was rewritten by a change of CL01 (%v → %v)", b.at, a.at)
	}
	if a, b := after[key("clf", "CL01", "Q3")], before[key("clf", "CL01", "Q3")]; a.value == b.value {
		t.Errorf("clf at CL01/Q3 did not change (%v)", a.value)
	}
	same("CL01 quantity")

	// Random changes: amounts, and the country a SUMIFS sorts lines by.
	for i := 0; i < 12; i++ {
		code := fmt.Sprintf("CL%02d", 1+rng.Intn(12))
		patch := map[string]any{}
		switch rng.Intn(3) {
		case 0:
			patch["quantity"] = float64(rng.Intn(60))
		case 1:
			patch["rate"] = float64(5 + rng.Intn(120))
		default:
			patch["geo"] = geos[rng.Intn(len(geos))]
		}
		change(code, patch)
		same(fmt.Sprintf("change %d: %s %v", i, code, patch))
	}
}
