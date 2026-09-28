package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

// TestPureRatioAverageReadsTheSchedulersAggregate: a calculated metric with
// agg_rule average whose formula does not depend on the members (a pure
// ratio) is, above the leaves, its formula evaluated AT the aggregate — the
// scheduler's useEval (tsEvaluator.finish, oneDimSliceRows) — never the mean
// of its leaf rows. Every reader follows the scheduler's rows:
//
//   - chart-data of a SERVED one (PREVIOUS(revenue)) reads the persisted row
//     at the point, a pin on the region's sole root (World) dropped, as the
//     grid's scope drops it; no row (EMEA × Feb) is no point, and a
//     restricted viewer whose grid withholds the total gets no World bar;
//   - a collapsed non-served one (revenue * 2) on a time dimension gets its
//     '{}' total row, which the scoped read at World equals;
//   - /api/grid flags both aggregate_evaluated, so the browser reads their
//     parents from the server instead of reducing leaf cells.
//
// World > {EMEA > {UK, DE}, US}; revenue at month m: UK 100+m, DE 200+m,
// US 300+m. prva at World over the months is Σ PREVIOUS(World revenue) =
// Σ_{m=1..5} (600+3m) = 3045; the mean of the leaf rows would be a third.
func TestPureRatioAverageReadsTheSchedulersAggregate(t *testing.T) {
	f := setupRestrictedFixture(t)
	dims := "/api/developer/dimensions/" + f.region + "/members"
	f.members["World"] = f.dev_("POST", dims, map[string]any{"code": "World", "label": "World"})
	f.dev_("PATCH", dims+"/"+f.members["EMEA"], map[string]any{"code": "EMEA", "label": "EMEA",
		"parent_member_id": f.members["World"]})
	f.dev_("PATCH", dims+"/"+f.members["US"], map[string]any{"code": "US", "label": "US",
		"parent_member_id": f.members["World"],
		"properties":       map[string]string{"segment": "ENT", "currency": "USD", "factor": "4"}})
	f.addMetric("prva", "PREVIOUS(revenue)", "average")
	f.addMetric("rav", "revenue * 2", "average")
	f.recalc()

	sum := func(from, to int, at func(m int) float64) float64 {
		s := 0.0
		for m := from; m <= to; m++ {
			s += at(m)
		}
		return s
	}
	world := sum(1, 5, func(m int) float64 { return 600 + 3*float64(m) }) // 3045
	emea := sum(1, 5, func(m int) float64 { return 300 + 2*float64(m) })  // 1530
	us := sum(1, 5, func(m int) float64 { return 300 + float64(m) })      // 1515
	revenueTotal := sum(1, 6, func(m int) float64 { return 600 + 3*float64(m) })
	f.await("prva", map[string]string{f.region: "World"}, world)
	f.await("prva", map[string]string{f.region: "EMEA"}, emea)
	f.await("prva", map[string]string{f.period: "2026-02"}, 603)
	f.await("prva", map[string]string{}, world)
	// The collapsed pure ratio's total: its one evaluation at '{}'.
	f.await("rav", map[string]string{}, 2*revenueTotal)

	// Scoped reads: the World scope is the unscoped total.
	totalsOnly := func(persona, scope string) gridRead {
		t.Helper()
		path := "/api/grid?totals_only=1&grid_def_id=" + f.plan
		if scope != "" {
			path += "&scope=" + url.QueryEscape(scope)
		}
		status, raw := f.req("GET", path, persona, nil)
		if status != http.StatusOK {
			t.Fatalf("%s: %d %s", path, status, raw)
		}
		var g gridRead
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	scope := func(code string) string { return fmt.Sprintf(`{%q:%q}`, f.region, code) }
	for label, g := range map[string]gridRead{
		"grid":                    f.grid(f.dev, f.plan, ""),
		"scope World totals_only": totalsOnly(f.dev, scope("World")),
	} {
		for name, want := range map[string]float64{"prva": world, "rav": 2 * revenueTotal} {
			if v, ok := g.Totals[f.metric[name]]; !ok || !nearly(v, want) {
				t.Errorf("%s: %s total = %v (present=%v), want %v", label, name, v, ok, want)
			}
		}
	}
	if v, ok := totalsOnly(f.dev, scope("EMEA")).Totals[f.metric["prva"]]; !ok || !nearly(v, emea) {
		t.Errorf("scope EMEA: prva = %v (present=%v), want %v", v, ok, emea)
	}

	// The grid tells the browser which metrics are evaluated at the aggregate.
	status, raw := f.req("GET", "/api/grid?meta_only=1&grid_def_id="+f.plan, f.dev, nil)
	if status != http.StatusOK {
		t.Fatalf("grid meta: %d %s", status, raw)
	}
	var meta struct {
		Metrics []struct {
			Name               string `json:"name"`
			AggregateEvaluated bool   `json:"aggregate_evaluated"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	flags := map[string]bool{}
	for _, m := range meta.Metrics {
		flags[m.Name] = m.AggregateEvaluated
	}
	for name, want := range map[string]bool{"prva": true, "rav": true, "revenue": false, "prev": false, "smb_rev": false} {
		if got, ok := flags[name]; !ok || got != want {
			t.Errorf("grid metric %s aggregate_evaluated = %v (listed=%v), want %v", name, got, ok, want)
		}
	}

	// Chart-data by region follows the scheduler's rows.
	dash := f.dev_("POST", "/api/developer/dashboards", map[string]any{"name": "P", "revision_id": f.revID})
	v := func(x float64) *float64 { return &x }
	all := f.chartPoints(f.dev, f.chartWidget(dash, f.region, []string{"prva"}, map[string]string{}))
	f.point("all periods", all, "prva", "World", v(world))
	f.point("all periods", all, "prva", "EMEA", v(emea))
	f.point("all periods", all, "prva", "US", v(us))
	feb := f.chartPoints(f.dev, f.chartWidget(dash, f.region, []string{"prva"}, map[string]string{f.period: "2026-02"}))
	f.point("February", feb, "prva", "World", v(603))
	f.point("February", feb, "prva", "EMEA", nil) // no persisted row: none, as the scoped read
	f.point("February", feb, "prva", "US", v(301))

	// A viewer with US hidden: the grid withholds the total, and the chart
	// shows no World bar — never a partial value over the visible leaves.
	f.hide("US")
	if g := f.grid(f.viewer, f.plan, ""); !slices.Contains(g.Withheld, f.metric["prva"]) {
		t.Errorf("viewer grid: prva total not withheld (totals %v)", g.Totals[f.metric["prva"]])
	}
	viewer := f.chartPoints(f.viewer, f.chartWidget(dash, f.region, []string{"prva"}, map[string]string{}))
	if got, plotted := viewer["prva"]["World"]; plotted && got != nil {
		t.Errorf("viewer chart: prva at World = %v, want null (withheld)", *got)
	}
}
