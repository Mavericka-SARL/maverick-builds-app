package main

import (
	"fmt"
	"strings"
)

// viewerGridProblems checks a restricted business user's grid (US hidden)
// against the withheld-cell contract: a cell whose read set touches US is
// listed in "withheld" and absent from cells; one that does not is served
// with its exact value; a total containing a withheld cell is withheld.
func viewerGridProblems(e *environment, g gridView, metric map[string]string) map[string][]string {
	out := map[string][]string{}
	if g.status != 200 {
		out["viewer grid read"] = []string{fmt.Sprintf("GET /api/grid -> %d: %s", g.status, clip(g.body, 300))}
		return out
	}
	usP := []string{}
	for k := range g.rawCells {
		for _, part := range strings.Split(k, ":")[1:] {
			if part == "US" {
				usP = append(usP, k)
			}
		}
	}
	out["viewer: no cell of the hidden member US is served"] = usP

	visible := []string{"DE", "UK"}
	revP := []string{}
	revTotal := 0.0
	for _, r := range visible {
		for _, m := range months {
			v, ok := g.cell(metric["revenue"], r, monthCode(m))
			expectVal(&revP, "revenue "+r+" "+monthCode(m), v, ok, rev(r, m))
			revTotal += rev(r, m)
		}
	}
	v, ok := g.totals[metric["revenue"]]
	expectVal(&revP, "revenue total (visible leaves)", v, ok, revTotal)
	out["viewer: input revenue cells and total over visible members"] = revP

	for _, d := range e.defs {
		mid := metric[d.name]
		p := []string{}
		sum := 0.0
		for i, r := range visible {
			for _, m := range months {
				key := normKey(mid, r, monthCode(m))
				v, inCells := g.cells[key]
				where := r + " " + monthCode(m)
				switch d.viewer[i] {
				case vPresent:
					want := d.leaf(e, r, m)
					sum += want
					expectVal(&p, where, v, inCells, want)
					if g.withheld[key] {
						p = append(p, where+" listed as withheld")
					}
				case vWithheld:
					if inCells {
						p = append(p, fmt.Sprintf("%s served (%g), want withheld", where, v))
					}
					if !g.withheld[key] {
						p = append(p, where+" not listed in withheld")
					}
				}
			}
		}
		out[fmt.Sprintf("viewer cells: %s [DE %c, UK %c]", d.name, d.viewer[0], d.viewer[1])] = p

		tp := []string{}
		tv, inTotals := g.totals[mid]
		switch d.viewerTotal {
		case 'S':
			expectVal(&tp, "total", tv, inTotals, sum)
			if g.withheld[mid] {
				tp = append(tp, "total listed as withheld")
			}
			out["viewer total: "+d.name+" = sum of visible leaves"] = tp
		case 'W':
			if inTotals {
				tp = append(tp, fmt.Sprintf("total served (%g), want withheld", tv))
			}
			if !g.withheld[mid] {
				tp = append(tp, "total not listed in withheld")
			}
			out["viewer total: "+d.name+" withheld"] = tp
		}
	}
	return out
}

func viewerChartExpect(e *environment) (region, segment, monthsExp []chartExpect) {
	const m3, m5 = 3, 5
	for _, r := range []string{"DE", "UK"} {
		region = append(region,
			chartExpect{"revenue", r, f(rev(r, m3))},
			chartExpect{"share_world", r, nil},
			chartExpect{"rev_usd", r, f(rev(r, m3) * fxRates[e.props[r].currency])},
			chartExpect{"fx_region", r, f(fxRates[e.props[r].currency])},
			chartExpect{"sum_de_code", r, f(rev("DE", m3))},
		)
		segment = append(segment,
			chartExpect{"ent_rev", r, nil},
			chartExpect{"cnt_ent", r, nil},
			chartExpect{"max_big", r, nil},
			chartExpect{"sumif_smb", r, f(rev("UK", m5))},
		)
	}
	segment = append(segment,
		chartExpect{"same_seg", "DE", nil},
		chartExpect{"same_seg", "UK", f(rev("UK", m5))},
	)
	for _, m := range months {
		for _, n := range []string{"qv", "hytd", "yv", "ts_q2", "lag_rev"} {
			monthsExp = append(monthsExp, chartExpect{n, monthCode(m), f(e.def(n).leaf(e, "DE", m))})
		}
	}
	return region, segment, monthsExp
}

func checkViewerCharts(e *environment, label string) {
	region, segment, monthsExp := viewerChartExpect(e)
	p := chartProblems(e, e.viewer, e.chartRegion, map[string]string{e.periodDim: "2026-03"}, region, []string{"US"})
	check(label+"chart-data by region: share_world withheld (null), FX LOOKUP for DE/UK exact, US not plotted", len(p) == 0, "%s", summarize(p))
	p = chartProblems(e, e.viewer, e.chartSegment, map[string]string{e.periodDim: "2026-05"}, segment, []string{"US"})
	check(label+"chart-data by region: SUMIFS touching US null, SMB/same-segment UK exact", len(p) == 0, "%s", summarize(p))
	p = chartProblems(e, e.viewer, e.chartMonths, map[string]string{e.regionDim: "DE"}, monthsExp, nil)
	check(label+"chart-data by month @DE: time functions exact", len(p) == 0, "%s", summarize(p))
}

func checkRestrictedViewer(e *environment) {
	step("business_admin hides member US from the business user")
	e.ba.call("PUT", "/api/business-admin/users/"+e.viewerUserID+"/access-rules", map[string]any{"rules": []map[string]string{
		{"rule_type": "dimension_member", "ref_id": e.member["US"], "access": "hidden"},
	}})

	report(viewerGridProblems(e, fetchGrid(e.viewer, e.planGrid, e.revA), e.metric))
	checkViewerCharts(e, "viewer ")

	p := []string{}
	for _, m := range e.viewer.list("GET", "/api/metrics?revision_id="+e.revA, nil) {
		if isInput, _ := m["is_input"].(bool); isInput {
			continue
		}
		if m["value"] != nil {
			p = append(p, fmt.Sprintf("%v = %v", m["name"], m["value"]))
		}
	}
	check("viewer /api/metrics: every calculated value is null", len(p) == 0, "%s", summarize(p))
}

// checkRevisionLineage creates a revision from the active one, activates it,
// and proves the business user's rule (hide US) still applies when reading
// the old revision — and applies in the new one too.
func checkRevisionLineage(e *environment) {
	step("developer creates revision \"Next\" from \"Working\" and activates it")
	revB := id(e.dev.call("POST", "/api/developer/revisions", map[string]any{"name": "Next", "source_revision_id": e.revA}))
	e.dev.call("PUT", "/api/developer/revisions/"+revB+"/activate", nil)

	res := viewerGridProblems(e, fetchGrid(e.viewer, e.planGrid, e.revA), e.metric)
	report(prefix("old revision: ", res))
	checkViewerCharts(e, "old revision: viewer ")

	// The new revision: its own copies of the grid and metrics.
	planB := ""
	for _, g := range e.dev.list("GET", "/api/developer/grids?revision_id="+revB, nil) {
		if g["name"] == "Plan" {
			planB, _ = g["id"].(string)
		}
	}
	if planB == "" {
		check("new revision has its copy of grid Plan", false, "not found in GET /api/developer/grids?revision_id=%s", revB)
		return
	}
	metricB := map[string]string{}
	for _, m := range e.dev.list("GET", "/api/metrics?revision_id="+revB, nil) {
		n, _ := m["name"].(string)
		metricB[n], _ = m["id"].(string)
	}
	subset := map[string]bool{"fx_region": true, "share_world": true, "sum_de_code": true}
	res = waitFor("new revision recalculation (viewer)", func() map[string][]string {
		all := viewerGridProblems(e, fetchGrid(e.viewer, planB, revB), metricB)
		out := map[string][]string{}
		for k, v := range all {
			keep := strings.HasPrefix(k, "viewer: ") || strings.HasPrefix(k, "viewer grid read")
			for n := range subset {
				if strings.Contains(k, " "+n+" ") || strings.HasSuffix(k, " "+n) {
					keep = true
				}
			}
			if keep {
				out[k] = v
			}
		}
		return out
	})
	report(prefix("new revision: ", res))
}
