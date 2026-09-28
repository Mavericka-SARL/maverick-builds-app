package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ── grid reads ──────────────────────────────────────────────────────────────

// gridView indexes a GET /api/grid response by metric and member codes,
// independent of the order the key lists the metric's dimensions in.
type gridView struct {
	cells    map[string]float64
	totals   map[string]float64
	withheld map[string]bool
	rawCells map[string]float64
	status   int
	body     string
}

func normKey(metricID string, codes ...string) string {
	c := append([]string(nil), codes...)
	sort.Strings(c)
	return metricID + "#" + strings.Join(c, "|")
}

func normRaw(k string) string {
	parts := strings.Split(k, ":")
	return normKey(parts[0], parts[1:]...)
}

func fetchGrid(a *api, gridID, revisionID string) gridView {
	return fetchGridScoped(a, gridID, revisionID, nil)
}

// fetchGridScoped reads a grid pinned to scope (dimension id -> member code),
// the dashboard's scoped read; a nil scope is the whole grid.
func fetchGridScoped(a *api, gridID, revisionID string, scope map[string]string) gridView {
	q := url.Values{}
	q.Set("grid_def_id", gridID)
	if revisionID != "" {
		q.Set("revision_id", revisionID)
	}
	if len(scope) > 0 {
		b, _ := json.Marshal(scope)
		q.Set("scope", string(b))
	}
	raw, status := a.raw("GET", "/api/grid?"+q.Encode(), nil)
	g := gridView{cells: map[string]float64{}, totals: map[string]float64{}, withheld: map[string]bool{}, status: status, body: string(raw)}
	if status != 200 {
		return g
	}
	var resp struct {
		Cells    map[string]float64 `json:"cells"`
		Totals   map[string]float64 `json:"totals"`
		Withheld []string           `json:"withheld"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		g.status = -1
		return g
	}
	g.rawCells = resp.Cells
	for k, v := range resp.Cells {
		g.cells[normRaw(k)] = v
	}
	for k, v := range resp.Totals {
		g.totals[k] = v
	}
	for _, k := range resp.Withheld {
		if strings.Contains(k, ":") {
			g.withheld[normRaw(k)] = true
		} else {
			g.withheld[k] = true
		}
	}
	return g
}

func (g gridView) cell(metricID string, codes ...string) (float64, bool) {
	v, ok := g.cells[normKey(metricID, codes...)]
	return v, ok
}

// waitFor polls until eval reports no problems or the timeout passes, and
// returns the last problems per check name.
func waitFor(what string, eval func() map[string][]string) map[string][]string {
	deadline := time.Now().Add(*flagTimeout)
	start := time.Now()
	var last map[string][]string
	for {
		last = eval()
		bad := 0
		for _, p := range last {
			if len(p) > 0 {
				bad++
			}
		}
		if bad == 0 {
			step("%s settled after %s", what, time.Since(start).Round(100*time.Millisecond))
			return last
		}
		if time.Now().After(deadline) {
			step("%s: %d checks still wrong after %s", what, bad, *flagTimeout)
			return last
		}
		time.Sleep(time.Second)
	}
}

func report(results map[string][]string) {
	names := make([]string, 0, len(results))
	for n := range results {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := results[n]
		if len(p) == 0 {
			check(n, true, "")
			continue
		}
		more := ""
		if len(p) > 4 {
			more = fmt.Sprintf(" (+%d more)", len(p)-4)
			p = p[:4]
		}
		check(n, false, "%s%s", strings.Join(p, "; "), more)
	}
}

func expectVal(problems *[]string, where string, got float64, ok bool, want float64) {
	switch {
	case !ok:
		*problems = append(*problems, fmt.Sprintf("%s missing, want %g", where, want))
	case !approx(got, want):
		*problems = append(*problems, fmt.Sprintf("%s = %g, want %g", where, got, want))
	}
}

// devGridProblems compares the developer's (unrestricted) grid with the
// independently computed values.
func devGridProblems(e *environment, only map[string]bool) map[string][]string {
	g := fetchGrid(e.dev, e.planGrid, e.revA)
	out := map[string][]string{}
	if g.status != 200 {
		out["grid read"] = []string{fmt.Sprintf("GET /api/grid -> %d: %s", g.status, clip(g.body, 300))}
		return out
	}
	revProblems := []string{}
	revTotal := 0.0
	for _, r := range leafRegions {
		for _, m := range months {
			v, ok := g.cell(e.metric["revenue"], r, monthCode(m))
			expectVal(&revProblems, "revenue "+r+" "+monthCode(m), v, ok, rev(r, m))
			revTotal += rev(r, m)
		}
	}
	v, ok := g.totals[e.metric["revenue"]]
	expectVal(&revProblems, "revenue total", v, ok, revTotal)
	if only == nil || only["revenue"] {
		out["grid: input revenue cells and total"] = revProblems
	}

	for _, d := range e.defs {
		if only != nil && !only[d.name] {
			continue
		}
		mid := e.metric[d.name]
		cellP := []string{}
		sum := 0.0
		for _, r := range leafRegions {
			for _, m := range months {
				want := d.leaf(e, r, m)
				sum += want
				v, ok := g.cell(mid, r, monthCode(m))
				expectVal(&cellP, r+" "+monthCode(m), v, ok, want)
			}
		}
		out[fmt.Sprintf("grid cells: %s = %s", d.name, d.formula)] = cellP
		want := sum
		if d.total != nil {
			want = d.total(e)
		}
		totP := []string{}
		v, ok := g.totals[mid]
		expectVal(&totP, "total", v, ok, want)
		if g.withheld[mid] {
			totP = append(totP, "total listed as withheld for an unrestricted developer")
		}
		out[fmt.Sprintf("grid total: %s (agg %s)", d.name, aggName(d))] = totP
	}
	if only == nil && len(g.withheld) > 0 {
		out["grid: nothing withheld from the developer"] = []string{fmt.Sprintf("%d keys withheld", len(g.withheld))}
	} else if only == nil {
		out["grid: nothing withheld from the developer"] = nil
	}
	return out
}

func aggName(d calcDef) string {
	if d.agg == "" {
		return "sum"
	}
	return d.agg
}

func checkDeveloperGrid(e *environment) {
	res := waitFor("developer grid recalculation", func() map[string][]string { return devGridProblems(e, nil) })
	report(res)

	// The bare-dimension formula at an aggregate member: EMEA is pinned
	// there, so region = "EMEA" holds and the scheduler's value is EMEA's
	// revenue. Reported only if the grid serves parent-member cells at all.
	g := fetchGrid(e.dev, e.planGrid, e.revA)
	if _, ok := g.cell(e.metric["emea_flag"], "EMEA", monthCode(1)); ok {
		p := []string{}
		for _, m := range months {
			v, ok := g.cell(e.metric["emea_flag"], "EMEA", monthCode(m))
			expectVal(&p, "EMEA "+monthCode(m), v, ok, rev("DE", m)+rev("UK", m))
		}
		check("grid cells: emea_flag at the EMEA aggregate member", len(p) == 0, "%s", strings.Join(p, "; "))
	} else {
		step("grid serves no parent-member cells for emea_flag; aggregate-member check covered by chart-data")
	}

	// /api/metrics for the developer: a calculated metric's value is the
	// whole-model total the scheduler persisted.
	for _, m := range e.dev.list("GET", "/api/metrics?revision_id="+e.revA, nil) {
		if m["id"] == e.metric["is_total"] {
			v, ok := m["value"].(float64)
			want := 0.0
			for _, r := range leafRegions {
				want += sumRev(r, 1, 12)
			}
			check("/api/metrics (developer): is_total whole-model value", ok && approx(v, want), "got %v, want %g", m["value"], want)
		}
	}
}

// ── chart-data ──────────────────────────────────────────────────────────────

type chartView struct {
	status  int
	body    string
	cats    []string
	values  map[string][]*float64 // metric id -> values aligned with cats
	context map[string]string
}

func fetchChart(a *api, widgetID string, ctx map[string]string) chartView {
	raw, status := a.raw("POST", "/api/dashboard-widgets/"+widgetID+"/chart-data", map[string]any{"context": ctx})
	c := chartView{status: status, body: string(raw), values: map[string][]*float64{}}
	if status != 200 {
		return c
	}
	var resp struct {
		Categories []struct {
			Key string `json:"key"`
		} `json:"categories"`
		Series []struct {
			MetricID string     `json:"metric_id"`
			Values   []*float64 `json:"values"`
		} `json:"series"`
		Context map[string]string `json:"context"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		c.status = -1
		return c
	}
	for _, k := range resp.Categories {
		c.cats = append(c.cats, k.Key)
	}
	for _, s := range resp.Series {
		c.values[s.MetricID] = s.Values
	}
	c.context = resp.Context
	return c
}

// point returns the value of metricID at category key: present=false when
// the category or series is missing, isNull when the point is null.
func (c chartView) point(metricID, key string) (v float64, present, isNull bool) {
	vals, ok := c.values[metricID]
	if !ok {
		return 0, false, false
	}
	for i, k := range c.cats {
		if k == key && i < len(vals) {
			if vals[i] == nil {
				return 0, true, true
			}
			return *vals[i], true, false
		}
	}
	return 0, false, false
}

func (c chartView) hasCat(key string) bool {
	for _, k := range c.cats {
		if k == key {
			return true
		}
	}
	return false
}

// chartExpect is one expected point: want == nil means null (withheld).
type chartExpect struct {
	metric, key string
	want        *float64
}

func f(v float64) *float64 { return &v }

func chartProblems(e *environment, a *api, widget string, ctx map[string]string, exp []chartExpect, absent []string) []string {
	c := fetchChart(a, widget, ctx)
	if c.status != 200 {
		return []string{fmt.Sprintf("POST chart-data -> %d: %s", c.status, clip(c.body, 300))}
	}
	p := []string{}
	for dim, code := range ctx {
		if c.context[dim] != code {
			p = append(p, fmt.Sprintf("resolved context %s=%q, asked %q", e.dimName(dim), c.context[dim], code))
		}
	}
	for _, x := range exp {
		v, present, isNull := c.point(e.metric[x.metric], x.key)
		where := x.metric + "@" + x.key
		switch {
		case !present:
			p = append(p, where+" missing")
		case x.want == nil && !isNull:
			p = append(p, fmt.Sprintf("%s = %g, want null (withheld)", where, v))
		case x.want != nil && isNull:
			p = append(p, fmt.Sprintf("%s = null, want %g", where, *x.want))
		case x.want != nil && !approx(v, *x.want):
			p = append(p, fmt.Sprintf("%s = %g, want %g", where, v, *x.want))
		}
	}
	for _, k := range absent {
		if c.hasCat(k) {
			p = append(p, "category "+k+" is plotted but must be hidden")
		}
	}
	return p
}

func (e *environment) dimName(dimID string) string {
	switch dimID {
	case e.regionDim:
		return "region"
	case e.periodDim:
		return "period"
	case e.currencyDim:
		return "currency"
	}
	return dimID
}

func devChartRegion(e *environment) []chartExpect {
	const m = 3
	exp := []chartExpect{}
	for _, r := range leafRegions {
		exp = append(exp,
			chartExpect{"revenue", r, f(rev(r, m))},
			chartExpect{"share_world", r, f(rev(r, m) / world(m))},
			chartExpect{"rev_usd", r, f(rev(r, m) * fxRates[e.props[r].currency])},
			chartExpect{"fx_region", r, f(fxRates[e.props[r].currency])},
			chartExpect{"sum_de_code", r, f(rev("DE", m))},
		)
	}
	emea := rev("DE", m) + rev("UK", m)
	exp = append(exp,
		chartExpect{"revenue", "EMEA", f(emea)},
		chartExpect{"revenue", "World", f(world(m))},
		// share_world has agg_rule formula: an aggregate member is the
		// scheduler's own ratio at that member, not a mean of its children.
		chartExpect{"share_world", "EMEA", f(emea / world(m))},
		chartExpect{"share_world", "AMER", f(rev("US", m) / world(m))},
		chartExpect{"share_world", "World", f(1)},
	)
	return exp
}

func devChartMonths(e *environment) []chartExpect {
	exp := []chartExpect{}
	for _, m := range months {
		for _, n := range []string{"qv", "hytd", "yv", "ts_q2", "lag_rev"} {
			exp = append(exp, chartExpect{n, monthCode(m), f(e.def(n).leaf(e, "DE", m))})
		}
	}
	return exp
}

func devChartSegment(e *environment) []chartExpect {
	const m = 5
	exp := []chartExpect{}
	for _, r := range leafRegions {
		for _, n := range []string{"ent_rev", "same_seg", "cnt_ent", "max_big", "sumif_smb"} {
			exp = append(exp, chartExpect{n, r, f(e.def(n).leaf(e, r, m))})
		}
	}
	return exp
}

func (e *environment) def(name string) calcDef {
	for _, d := range e.defs {
		if d.name == name {
			return d
		}
	}
	fatalf("no calculated metric %s", name)
	return calcDef{}
}

func checkDeveloperCharts(e *environment) {
	for _, c := range []struct {
		name   string
		widget string
		ctx    map[string]string
		exp    []chartExpect
	}{
		{"chart-data by region @2026-03: revenue, share_world, rev_usd, fx_region, sum_de_code", e.chartRegion,
			map[string]string{e.periodDim: "2026-03"}, devChartRegion(e)},
		{"chart-data by month @DE: qv, hytd, yv, ts_q2, lag_rev", e.chartMonths,
			map[string]string{e.regionDim: "DE"}, devChartMonths(e)},
		{"chart-data by region @2026-05: ent_rev, same_seg, cnt_ent, max_big, sumif_smb", e.chartSegment,
			map[string]string{e.periodDim: "2026-05"}, devChartSegment(e)},
	} {
		p := chartProblems(e, e.dev, c.widget, c.ctx, c.exp, nil)
		check(c.name, len(p) == 0, "%s", summarize(p))
	}
}

func summarize(p []string) string {
	if len(p) > 4 {
		return strings.Join(p[:4], "; ") + fmt.Sprintf(" (+%d more)", len(p)-4)
	}
	return strings.Join(p, "; ")
}

// ── save-time validation ────────────────────────────────────────────────────

func checkBadFormulas(e *environment) {
	for i, c := range []struct{ formula, code string }{
		{`LOOKUP(revenue * 2, region, "DE")`, "SOURCE_MUST_BE_METRIC"},
		{`SUMIFS(revenue * 2, region, "DE")`, "SOURCE_MUST_BE_METRIC"},
		{`YEARVALUE(revenue * 2)`, "SOURCE_MUST_BE_METRIC"},
		{`LOOKUP(revenue, region, "XX")`, "UNKNOWN_MEMBER"},
		{`LOOKUP(fx_rate, region, "DE")`, "DIMENSION_NOT_ON_SOURCE"},
		{`LOOKUP(revenue, revenue, "DE")`, "DIMENSION_ARGUMENT_REQUIRED"},
		{`revenue * region.nosuch`, "UNKNOWN_PROPERTY"},
		{`LAG(revenue, 1.5, 0)`, "TIME_OFFSET_NOT_INTEGER"},
		{`MOVINGSUM(revenue, 0 - 2, 0)`, "MOVING_WINDOW_NOT_LITERAL"},
		{`WEEKVALUE(revenue)`, ""},
	} {
		raw, status := e.dev.try("POST", "/api/developer/metrics", map[string]any{
			"name": fmt.Sprintf("bad_%d", i), "is_input": false, "formula": "=" + c.formula, "revision_id": e.revA,
		})
		want := c.code
		if want == "" {
			want = "(any)"
		}
		ok := status == 400 && (c.code == "" || strings.Contains(raw, c.code))
		check(fmt.Sprintf("save refuses %s -> 400 %s", c.formula, want), ok, "got %d %s", status, clip(raw, 240))
	}

	// Checks that need the metric's own time axis run on a placed metric.
	for _, c := range []struct{ metric, formula, code string }{
		{"ts_q2", `TIMESUM(revenue, "2030-01", "Q1")`, "UNKNOWN_MEMBER"},
		{"lag_rev", `LAG(lag_rev, 1, 0) + LOOKUP(revenue, region, "DE")`, "TEMPORAL_CYCLE_NOT_CAUSAL"},
	} {
		d := e.def(c.metric)
		raw, status := e.dev.try("PATCH", "/api/developer/metrics/"+e.metric[c.metric], map[string]any{
			"name": c.metric, "formula": "=" + c.formula, "agg_rule": aggName(d),
		})
		ok := status == 400 && strings.Contains(raw, c.code)
		check(fmt.Sprintf("save refuses %s on placed %s -> 400 %s", c.formula, c.metric, c.code), ok, "got %d %s", status, clip(raw, 240))
		got := e.formulaOf(e.dev, c.metric)
		check(fmt.Sprintf("refused edit left %s's formula unchanged", c.metric), strings.Contains(got, d.formula), "formula now %q", got)
	}
}

// formulaOf reads a metric's formula from /api/metrics.
func (e *environment) formulaOf(a *api, name string) string {
	for _, m := range a.list("GET", "/api/metrics?revision_id="+e.revA, nil) {
		if m["id"] == e.metric[name] {
			s, _ := m["formula"].(string)
			return s
		}
	}
	return ""
}

// ── property declarations ───────────────────────────────────────────────────

func checkProperties(e *environment) {
	props := "/api/developer/dimensions/" + e.regionDim + "/properties"
	for _, c := range []struct {
		name, typ, code string
	}{
		{"bad name", "text", "INVALID_PROPERTY_NAME"},
		{"SEGMENT", "text", "PROPERTY_NAME_TAKEN"},
		{"flag", "bool", "INVALID_PROPERTY_TYPE"},
	} {
		raw, status := e.dev.try("POST", props, map[string]any{"name": c.name, "data_type": c.typ})
		check(fmt.Sprintf("property %q (%s) -> 400 %s", c.name, c.typ, c.code), status == 400 && strings.Contains(raw, c.code),
			"got %d %s", status, clip(raw, 200))
	}

	step("renaming property factor -> weight")
	e.dev.call("PATCH", props+"/"+e.propID["factor"], map[string]any{"name": "weight"})
	for _, n := range []string{"rev_factor", "max_big"} {
		got := e.formulaOf(e.dev, n)
		check("rename rewrote "+n+"'s formula to region.weight",
			strings.Contains(got, "region.weight") && !strings.Contains(got, "region.factor"), "formula now %q", got)
	}
	for i := range e.defs {
		e.defs[i].formula = strings.ReplaceAll(e.defs[i].formula, "region.factor", "region.weight")
	}

	step("setting UK's weight to 4 (a member property edit triggers recalculation)")
	uk := regionTree[4]
	e.setMemberProps(uk.code, uk.label, uk.parent, map[string]string{"weight": "4"})
	e.props["UK"].factor = 4
	res := waitFor("recalculation after the rename and the UK edit", func() map[string][]string {
		return devGridProblems(e, map[string]bool{"rev_factor": true, "max_big": true})
	})
	report(prefix("after rename: ", res))

	raw, status := e.dev.try("DELETE", props+"/"+e.propID["segment"], nil)
	check("deleting property segment while formulas read it -> 409 PROPERTY_IN_USE naming a metric",
		status == 409 && strings.Contains(raw, "PROPERTY_IN_USE") && strings.Contains(raw, "ent_rev"), "got %d %s", status, clip(raw, 300))

	spare := id(e.dev.call("POST", props, map[string]any{"name": "spare", "data_type": "date"}))
	raw, status = e.dev.try("DELETE", props+"/"+spare, nil)
	check("deleting an unused property succeeds", status == 200 || status == 204, "got %d %s", status, clip(raw, 200))
}

func prefix(p string, m map[string][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[p+k] = v
	}
	return out
}
