package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/query"
)

// TestRateTotalsAreNumeratorOverDenominator: a metric totalled as a rate
// (agg_rule "rate") is, at every total and subtotal a grid shows, its
// numerator over its denominator — whatever its own formula is, and for an
// input metric too, which has none. A tester saw the operands saved in the
// metric list but empty in the grid read, no total and "—" on the Total row
// (2026-11-24). Before the fix: the grid read carried neither operand, an
// input's total was the plain sum of its cells (1,000 × 4 = 4,000), its
// subtotals were blank, and a filtered read evaluated a calculated rate's own
// formula at the total (margin 200,000 for the UK) where the unfiltered one
// divided revenue by units.
func TestRateTotalsAreNumeratorOverDenominator(t *testing.T) {
	d := setupSalesDemo(t)
	rate := func(name string, isInput bool, formula string) {
		t.Helper()
		res := d.call("POST", "/api/developer/metrics", d.dev, map[string]any{
			"name": name, "is_input": isInput, "formula": formula, "revision_id": d.revID,
			"agg_rule": "rate", "format": "number",
			"agg_numerator_metric_id": d.metric["revenue"], "agg_denominator_metric_id": d.metric["units"],
		})
		d.metric[name] = idOf(res)
		if st, raw := d.req("POST", "/api/developer/grids/"+d.gridID+"/metrics/"+d.metric[name], d.dev, nil); st >= 300 {
			t.Fatalf("add %s to the grid: %d %s", name, st, raw)
		}
	}
	rate("typed_price", true, "")                        // a price entered per cell
	rate("price_by_margin", false, "={margin}")          // a formula that is not the ratio
	rate("price_lagged", false, "=LAG({revenue}, 1, 0)") // a time-series formula
	d.seedFacts()
	for _, f := range [][3]string{{"UK", "LAPTOP", "Q1"}, {"DE", "LAPTOP", "Q1"}, {"US", "LICENSE", "Q1"}, {"CA", "LICENSE", "Q2"}} {
		if st, raw := d.writeCell(d.dev, "typed_price", f[0], f[1], f[2], 1000); st >= 300 {
			t.Fatalf("enter a price: %d %s", st, raw)
		}
	}
	// Revenue 1.9m over 1,250 units.
	for _, m := range []string{"avg_price", "price_by_margin", "price_lagged"} {
		d.awaitCalc(m, map[string]string{}, 1_900_000.0/1250.0)
	}

	type read struct {
		Metrics []struct {
			ID  string `json:"id"`
			Num string `json:"agg_numerator_metric_id"`
			Den string `json:"agg_denominator_metric_id"`
		} `json:"metrics"`
		AllMetrics []struct {
			ID   string   `json:"id"`
			Dims []string `json:"dimension_ids"`
		} `json:"all_metrics"`
		Cells  map[string]float64 `json:"cells"`
		Totals map[string]float64 `json:"totals"`
	}
	get := func(query string) read {
		t.Helper()
		st, raw := d.req("GET", "/api/grid?grid_id="+d.gridID+query, d.dev, nil)
		var r read
		if st != 200 || json.Unmarshal(raw, &r) != nil {
			t.Fatalf("grid read %q: %d %s", query, st, raw)
		}
		return r
	}
	cellAt := func(r read, metricID string, geo, prod, period string) (float64, bool) {
		codes := map[string]string{d.geoDim: geo, d.prodDim: prod, d.periodDim: period}
		for _, am := range r.AllMetrics {
			if am.ID == metricID {
				key := metricID
				for _, dim := range am.Dims {
					key += ":" + codes[dim]
				}
				v, ok := r.Cells[key]
				return v, ok
			}
		}
		return 0, false
	}
	ukPin := "&scope=" + url.QueryEscape(fmt.Sprintf(`{%q:%q}`, d.geoDim, "UK"))
	reads := map[string]read{
		"unfiltered":              get(""),
		"totals only":             get("&totals_only=1"),
		"filtered to UK":          get(ukPin),
		"totals only, UK (slice)": get("&totals_only=1" + ukPin),
	}
	wantTotal := map[string]float64{
		"unfiltered": 1520, "totals only": 1520, // 1.9m / 1,250
		"filtered to UK": 1000, "totals only, UK (slice)": 1000, // 900k / 900
	}
	for _, name := range []string{"avg_price", "typed_price", "price_by_margin", "price_lagged"} {
		id := d.metric[name]
		for label, r := range reads {
			if got, ok := r.Totals[id]; !ok || !nearly(got, wantTotal[label]) {
				t.Errorf("%s, %s: total %v (present %v), want %v", name, label, got, ok, wantTotal[label])
			}
		}
		r := reads["unfiltered"]
		for _, m := range r.Metrics {
			if m.ID == id && (m.Num != d.metric["revenue"] || m.Den != d.metric["units"]) {
				t.Errorf("%s: the grid read carries operands %q / %q, want revenue / units", name, m.Num, m.Den)
			}
		}
		// Subtotals: EMEA laptops in Q1 are (900k + 500k) / (900 + 100);
		// UK laptops over H1 are 900k / 900.
		if got, ok := cellAt(r, id, "EMEA", "LAPTOP", "Q1"); !ok || !nearly(got, 1400) {
			t.Errorf("%s at EMEA × Laptop × Q1: %v (present %v), want 1,400", name, got, ok)
		}
		if got, ok := cellAt(r, id, "UK", "LAPTOP", "H1"); !ok || !nearly(got, 1000) {
			t.Errorf("%s at UK × Laptop × H1: %v (present %v), want 1,000", name, got, ok)
		}
		if got, ok := cellAt(reads["filtered to UK"], id, "UK", "LAPTOP", "H1"); !ok || !nearly(got, 1000) {
			t.Errorf("%s at UK × Laptop × H1, filtered to UK: %v (present %v), want 1,000", name, got, ok)
		}
	}
	// An input rate keeps its cells as entered.
	if got, ok := cellAt(reads["unfiltered"], d.metric["typed_price"], "UK", "LAPTOP", "Q1"); !ok || got != 1000 {
		t.Errorf("typed_price at UK × Laptop × Q1: %v (present %v), want the 1,000 entered", got, ok)
	}

	// Charts and the chat connector (the chart resolver, /api/grid/series)
	// read the same numbers: by geography, laptops in Q1 — EMEA is
	// 1.4m / 1,000; a leaf is its own value (the typed price, the formula).
	// They read an input rate as the mean of its children and a calculated
	// one as its formula at the parent (2026-11-24).
	ids := []string{}
	for _, name := range []string{"avg_price", "typed_price", "price_by_margin"} {
		ids = append(ids, d.metric[name])
	}
	sq := url.Values{
		"grid_def_id": {d.gridID}, "dimension_id": {d.geoDim}, "metric_ids": {strings.Join(ids, ",")},
		"context": {fmt.Sprintf(`{%q:%q,%q:%q}`, d.prodDim, "LAPTOP", d.periodDim, "Q1")},
	}
	st, raw := d.req("GET", "/api/grid/series?"+sq.Encode(), d.dev, nil)
	var series struct {
		Categories []struct{ Key string } `json:"categories"`
		Series     []struct {
			MetricID string     `json:"metric_id"`
			Values   []*float64 `json:"values"`
		} `json:"series"`
	}
	if st != 200 || json.Unmarshal(raw, &series) != nil {
		t.Fatalf("series: %d %s", st, raw)
	}
	// Above the leaves (EMEA) every rate is 1.4m / 1,000; at a leaf, each
	// metric's own value: revenue / units, the price typed, the margin.
	wantAt := map[string]map[string]float64{
		d.metric["avg_price"]:       {"EMEA": 1400, "UK": 1000, "DE": 5000},
		d.metric["typed_price"]:     {"EMEA": 1400, "UK": 1000, "DE": 1000},
		d.metric["price_by_margin"]: {"EMEA": 1400, "UK": 200_000, "DE": 200_000},
	}
	for _, s := range series.Series {
		for i, c := range series.Categories {
			want, check := wantAt[s.MetricID][c.Key]
			if !check {
				continue
			}
			if s.Values[i] == nil || !nearly(*s.Values[i], want) {
				got := "none"
				if s.Values[i] != nil {
					got = fmt.Sprint(*s.Values[i])
				}
				t.Errorf("chart series of %s at %s: %s, want %v", s.MetricID, c.Key, got, want)
			}
		}
	}
	// Workflow conditions and the AI sheet check (MetricValuesAt): the
	// grand total.
	vals, err := query.NewChartResolver(d.pool).MetricValuesAt(context.Background(), d.modelID, d.revID,
		[]string{"typed_price", "price_by_margin", "price_lagged"}, map[string]string{})
	if err != nil {
		t.Fatalf("MetricValuesAt: %v", err)
	}
	for name, v := range vals {
		if n, ok := v.Number(); !ok || !nearly(n, 1520) {
			t.Errorf("MetricValuesAt %s at the total: %v, want 1,520", name, v)
		}
	}

	// A change to the denominator alone recalculates a rate whose formula
	// does not read it: 1.9m over 1,250 - 100 + 400 units.
	if st, raw := d.writeCell(d.dev, "units", "DE", "LAPTOP", "Q1", 400); st >= 300 {
		t.Fatalf("enter units: %d %s", st, raw)
	}
	d.awaitCalc("price_by_margin", map[string]string{}, 1_900_000.0/1550.0)
}
