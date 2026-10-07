package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Regressions from the final review of the dimensional-references change,
// each proved live over HTTP. They are built in a fresh revision
// "Regressions" (which starts as a copy of the active one; everything here
// uses its own dimensions and names), so the member delete and the refused
// activation at the end cannot disturb the checks above.
//
//	F1  a dim.property metric has no cell at a member without the property,
//	    in a scoped grid read too, and the average total leaves it out;
//	F2  chart-data at a parent combines the leaves of a dim.property metric;
//	F3  LOOKUP / plain reference of an agg_rule=formula source at a cell
//	    that pins only some of its dimensions evaluates its formula there;
//	F4  YEARVALUE / LOOKUP to FY of a last/average source at a parent skips
//	    the periods with no data;
//	F5  SUMIFS along a related dimension recalculates: a parent dimension
//	    (parent_dimension_id) when a member moves to another parent, and a
//	    property grouping (source_dimension_id + source_property, created
//	    over HTTP with derive_members) when a member's property changes —
//	    SUMIFS and a grid by the grouping's totals both follow;
//	F6  deleting the member a literal LOOKUP names -> 409 MEMBER_IN_USE, the
//	    cells still served; after a formula-only PATCH the delete succeeds
//	    (and F8: a member rename still leaves the formula naming the old
//	    code, so activation refuses the revision with UNKNOWN_MEMBER);
//	F10 deleting a dimension a formula names -> 409 DIMENSION_IN_USE;
//	F12 a developer with a hidden member gets 403 from debug/calc?dim_members=
//	    and no hidden member's facts from debug/facts.

var regTree = []struct{ code, label, parent string }{
	{"World", "World", ""},
	{"EMEA", "EMEA", "World"},
	{"AMER", "Americas", "World"},
	{"DE", "Germany", "EMEA"},
	{"UK", "United Kingdom", "EMEA"},
	{"FR", "France", "EMEA"},
	{"US", "United States", "AMER"},
}

var (
	regLeaves = []string{"DE", "UK", "FR", "US"}
	// regFactor is region.factor; FR has none (F1).
	regFactor = map[string]float64{"DE": 3, "UK": 2, "US": 4}
	// F3: amount and units of product P1 by (region, channel).
	cubeAmount = map[[2]string]float64{{"DE", "Online"}: 600, {"DE", "Retail"}: 400, {"FR", "Online"}: 300, {"US", "Retail"}: 50}
	cubeUnits  = map[[2]string]float64{{"DE", "Online"}: 50, {"DE", "Retail"}: 51, {"FR", "Online"}: 4, {"US", "Retail"}: 4}
	// F4: bal (time_summary last) and avgp (average) at DE, January to March only.
	balDE = map[int]float64{1: 10, 2: 20, 3: 30}
	// F5: salary by department, and each department's parent in area.
	salary   = map[string]float64{"D1": 100, "D2": 200, "D3": 50}
	deptArea = map[string]string{"D1": "North", "D2": "South", "D3": "North"}
	// F5 (grouping): pay by employee, and each employee's team property;
	// the dimension team groups emp by it.
	pay     = map[string]float64{"P1": 10, "P2": 20, "P3": 5}
	empTeam = map[string]string{"P1": "North", "P2": "South", "P3": "North"}
	// F6: sales by zone.
	zoneSales = map[string]float64{"Z1": 10, "Z2": 500}
)

// regRev is revenue[region, month] in the Regressions revision.
func regRev(r string, m int) float64 {
	if r == "FR" {
		return 80 + 4*float64(m-1)
	}
	return rev(r, m)
}

// leavesUnder lists regTree's leaves beneath code (code itself for a leaf).
func leavesUnder(code string) []string {
	var out []string
	for _, m := range regTree {
		if m.parent == code {
			out = append(out, leavesUnder(m.code)...)
		}
	}
	if len(out) == 0 {
		return []string{code}
	}
	return out
}

// factorMean is the mean factor of the leaves that have one.
func factorMean(leaves []string) float64 {
	s, n := 0.0, 0
	for _, l := range leaves {
		if f, ok := regFactor[l]; ok {
			s += f
			n++
		}
	}
	return s / float64(n)
}

// wsumOf is revenue * factor summed over the leaves that have a factor.
func wsumOf(leaves []string, m int) float64 {
	s := 0.0
	for _, l := range leaves {
		if f, ok := regFactor[l]; ok {
			s += regRev(l, m) * f
		}
	}
	return s
}

// cubeRatio is summed amount over summed units of P1 across every channel
// of the given regions.
func cubeRatio(regions []string) float64 {
	in := map[string]bool{}
	for _, r := range regions {
		in[r] = true
	}
	a, u := 0.0, 0.0
	for k, v := range cubeAmount {
		if in[k[0]] {
			a += v
			u += cubeUnits[k]
		}
	}
	return a / u
}

// balLast and balMean: the closing and the average of the recorded months.
func balLast() float64 {
	last := 0
	for m := range balDE {
		if m > last {
			last = m
		}
	}
	return balDE[last]
}

func balMean() float64 {
	s := 0.0
	for _, v := range balDE {
		s += v
	}
	return s / float64(len(balDE))
}

// teamPay is the pay of the employees whose team property is team.
func teamPay(team string) float64 {
	s := 0.0
	for p, v := range pay {
		if empTeam[p] == team {
			s += v
		}
	}
	return s
}

func northSalary() float64 {
	s := 0.0
	for d, v := range salary {
		if deptArea[d] == "North" {
			s += v
		}
	}
	return s
}

type regEnv struct {
	*environment
	main                                     *environment
	productDim, channelDim, areaDim, deptDim string
	empDim, teamDim, people, teams           string // F5 grouping: dims, grids
	zoneDim, tierDim                         string
	props, cube, summary, staff, zones       string // grids
	chartProps, chartSummary                 string
	dev2ID                                   string
	dev2                                     *api
}

// grid creates a grid on the given dimensions in the environment's revision.
func (e *environment) grid(name string, dims ...string) string {
	g := id(e.dev.call("POST", "/api/developer/grids", map[string]any{"name": name, "revision_id": e.revA}))
	for _, d := range dims {
		e.dev.call("POST", "/api/developer/grids/"+g+"/dimensions/"+d, nil)
	}
	return g
}

func (e *environment) input(name, gridID, summary string) {
	body := map[string]any{"name": name, "is_input": true, "revision_id": e.revA}
	if summary != "" {
		body["time_summary"] = summary
	}
	e.metric[name] = id(e.dev.call("POST", "/api/developer/metrics", body))
	e.dev.call("POST", "/api/developer/grids/"+gridID+"/metrics/"+e.metric[name], nil)
}

// calc creates a calculated metric and places it on gridID ("" = unplaced).
func (e *environment) calc(name, formula, agg, gridID string) {
	if agg == "" {
		agg = "sum"
	}
	raw, status := e.dev.try("POST", "/api/developer/metrics", map[string]any{
		"name": name, "is_input": false, "formula": "=" + formula, "agg_rule": agg, "revision_id": e.revA,
	})
	if status != 200 {
		fatalf("create calculated metric %s = %s -> %d: %s", name, formula, status, clip(raw, 600))
	}
	e.metric[name] = idFromRaw(raw)
	if gridID == "" {
		return
	}
	if raw, status := e.dev.try("POST", "/api/developer/grids/"+gridID+"/metrics/"+e.metric[name], nil); status != 200 {
		fatalf("place %s -> %d: %s", name, status, clip(raw, 600))
	}
}

func buildRegressions(e *environment) *regEnv {
	step("developer creates revision \"Regressions\" and activates it")
	revR := id(e.dev.call("POST", "/api/developer/revisions", map[string]any{"name": "Regressions"}))
	e.dev.call("PUT", "/api/developer/revisions/"+revR+"/activate", nil)
	r := &regEnv{main: e, environment: &environment{
		root: e.root, dev: e.dev, ba: e.ba, viewer: e.viewer,
		tenantID: e.tenantID, workspaceID: e.workspaceID, appID: e.appID, modelID: e.modelID,
		baUserID: e.baUserID, viewerUserID: e.viewerUserID, revA: revR,
		member: map[string]string{}, propID: map[string]string{}, metric: map[string]string{},
	}}

	step("geo (World > EMEA > DE, UK, FR; World > AMER > US) with number property factor; FR has none")
	r.regionDim = r.dim("geo", nil)
	for _, m := range regTree {
		r.addMember(r.regionDim, m.code, m.label, m.parent, nil)
	}
	r.propID["factor"] = id(r.dev.call("POST", "/api/developer/dimensions/"+r.regionDim+"/properties",
		map[string]any{"name": "factor", "data_type": "number"}))
	for _, m := range regTree {
		if f, ok := regFactor[m.code]; ok {
			r.setMemberProps(m.code, m.label, m.parent, map[string]string{"factor": strconv.FormatFloat(f, 'f', -1, 64)})
		}
	}
	r.buildPeriod("month")

	step("grid Props [geo, month]: g_rev, bal (last), avgp (average); wavg, wsum, yvl, yva, fyl")
	r.props = r.grid("Props", r.regionDim, r.periodDim)
	r.input("g_rev", r.props, "sum")
	r.input("bal", r.props, "last")
	r.input("avgp", r.props, "average")
	r.calc("wavg", "geo.factor", "average", r.props)
	r.calc("wsum", "g_rev * geo.factor", "sum", r.props)
	r.calc("yvl", "YEARVALUE(bal)", "formula", r.props)
	r.calc("yva", "YEARVALUE(avgp)", "formula", r.props)
	r.calc("fyl", `LOOKUP(bal, month, "FY26")`, "formula", r.props)

	step("product (P1, P2) and channel (Online, Retail); grids Cube [geo, product, channel] and Summary [geo, product]")
	r.productDim = r.dim("product", nil)
	r.addMember(r.productDim, "P1", "Product 1", "", nil)
	r.addMember(r.productDim, "P2", "Product 2", "", nil)
	r.channelDim = r.dim("channel", nil)
	r.addMember(r.channelDim, "Online", "Online", "", nil)
	r.addMember(r.channelDim, "Retail", "Retail", "", nil)
	r.cube = r.grid("Cube", r.regionDim, r.productDim, r.channelDim)
	r.input("amount", r.cube, "")
	r.input("units", r.cube, "")
	r.calc("price", "amount / units", "formula", r.cube)
	r.summary = r.grid("Summary", r.regionDim, r.productDim)
	r.calc("world_price", `LOOKUP(price, geo, "World")`, "formula", r.summary)
	r.calc("plain_price", "price", "formula", r.summary)

	step("area (North, South) and dept (D1..D3) whose parent dimension is area; grid Staff [dept]")
	r.areaDim = r.dim("area", nil)
	r.addMember(r.areaDim, "North", "North", "", nil)
	r.addMember(r.areaDim, "South", "South", "", nil)
	r.deptDim = r.dim("dept", map[string]any{"parent_dimension_id": r.areaDim})
	for _, d := range []string{"D1", "D2", "D3"} {
		r.addMember(r.deptDim, d, "Department "+d, deptArea[d], nil)
	}
	r.staff = r.grid("Staff", r.deptDim)
	r.input("salary", r.staff, "")
	r.calc("north_sal", `SUMIFS(salary, area, "North")`, "", r.staff)

	step("emp (P1..P3) with declared text property team; team GROUPS emp by it (source_dimension_id + source_property, derive_members); grids People [emp], Teams [team]")
	r.empDim = r.dim("emp", nil)
	r.propID["team"] = id(r.dev.call("POST", "/api/developer/dimensions/"+r.empDim+"/properties",
		map[string]any{"name": "team", "data_type": "text"}))
	for _, p := range []string{"P1", "P2", "P3"} {
		r.addMember(r.empDim, p, "Employee "+p, "", nil)
		r.setEmpTeam(p)
	}
	raw, status := r.dev.try("POST", "/api/developer/dimensions", map[string]any{
		"name": "team", "agg_rule": "sum", "revision_id": r.revA,
		"source_dimension_id": r.empDim, "source_property": "team", "derive_members": true,
	})
	if status != 200 {
		fatalf("create the property-grouping dimension team -> %d: %s", status, clip(raw, 600))
	}
	r.teamDim = idFromRaw(raw)
	var created struct {
		Derived []string `json:"derived_members"`
	}
	_ = json.Unmarshal([]byte(raw), &created)
	check("F5 grouping: POST team {source_dimension_id: emp, source_property: team, derive_members} derives North, South",
		strings.Join(created.Derived, ",") == "North,South", "derived_members = %v", created.Derived)
	r.people = r.grid("People", r.empDim)
	r.input("pay", r.people, "")
	r.calc("north_pay", `SUMIFS(pay, team, "North")`, "", r.people)
	r.teams = r.grid("Teams", r.teamDim)
	r.calc("team_pay", "pay", "", r.teams)

	step("zone (Z1, Z2), grid Zones [zone]: sales, lk_z2 = LOOKUP(sales, zone, \"Z2\")")
	r.zoneDim = r.dim("zone", nil)
	r.addMember(r.zoneDim, "Z1", "Zone 1", "", nil)
	r.addMember(r.zoneDim, "Z2", "Zone 2", "", nil)
	r.zones = r.grid("Zones", r.zoneDim)
	r.input("sales", r.zones, "")
	r.calc("lk_z2", `LOOKUP(sales, zone, "Z2")`, "", r.zones)

	step("tier (Gold, Silver) on no grid, named by the unplaced tier_n = COUNTIFS(tier, \"Gold\")")
	r.tierDim = r.dim("tier", nil)
	r.addMember(r.tierDim, "Gold", "Gold", "", nil)
	r.addMember(r.tierDim, "Silver", "Silver", "", nil)
	r.calc("tier_n", `COUNTIFS(tier, "Gold")`, "", "")

	step("dashboard: chart by geo on Props and on Summary")
	r.dashboard = id(r.dev.call("POST", "/api/developer/dashboards", map[string]any{"name": "Regressions", "revision_id": revR}))
	r.chartProps = r.addGridChart(r.props, "bar", r.regionDim, []string{"wavg", "wsum", "yvl", "yva", "fyl"},
		map[string]string{r.periodDim: "2026-02"}, 0)
	r.chartSummary = r.addGridChart(r.summary, "bar", r.regionDim, []string{"world_price", "plain_price"},
		map[string]string{r.productDim: "P1"}, 1)

	step("writing cube, bal/avgp at DE Jan..Mar, salaries, zone sales, then g_rev for 4 regions x 12 months")
	for k, v := range cubeAmount {
		dims := map[string]string{r.regionDim: k[0], r.productDim: "P1", r.channelDim: k[1]}
		r.putFact("amount", dims, v)
		r.putFact("units", dims, cubeUnits[k])
	}
	for m, v := range balDE {
		dims := map[string]string{r.regionDim: "DE", r.periodDim: monthCode(m)}
		r.putFact("bal", dims, v)
		r.putFact("avgp", dims, v)
	}
	for d, v := range salary {
		r.putFact("salary", map[string]string{r.deptDim: d}, v)
	}
	for p, v := range pay {
		r.putFact("pay", map[string]string{r.empDim: p}, v)
	}
	for z, v := range zoneSales {
		r.putFact("sales", map[string]string{r.zoneDim: z}, v)
	}
	// Written last so the debug facts listing (the latest 50) holds them,
	// US among them (F12).
	for _, l := range regLeaves {
		for _, m := range months {
			r.putFact("g_rev", map[string]string{r.regionDim: l, r.periodDim: monthCode(m)}, regRev(l, m))
		}
	}
	step("re-activating the revision to recalculate it as a whole")
	r.dev.call("PUT", "/api/developer/revisions/"+revR+"/activate", nil)
	return r
}

func checkRegressions(e *environment) {
	r := buildRegressions(e)

	section("F1: a dim.property metric at a member without the property")
	r.checkBlankProperty()

	section("F2: chart-data of dim.property metrics at parents")
	r.checkPropertyChart()

	section("F12: debug views for a developer with a hidden member")
	r.checkDebugViews()

	section("F3: LOOKUP of an agg_rule=formula source at partial coordinates")
	r.checkPartialFormulaSource()

	section("F4: YEARVALUE / LOOKUP to FY at a parent skips empty periods")
	r.checkEmptyPeriodsAtParent()

	section("F5: SUMIFS along a related dimension after a member moves")
	r.checkRelatedDimensionRecalc()

	section("F10: deleting a dimension a formula names")
	r.checkDimensionInUse()

	section("F6/F8: deleting / renaming the member a literal LOOKUP names")
	r.checkDeletedLookupMember()
}

// ── F1 ──────────────────────────────────────────────────────────────────────

// blankPropertyProblems checks wavg (= region.factor, agg average) and wsum
// (= revenue * region.factor, agg sum) over the leaves a read covers: FR has
// no wavg cell (and wsum none or 0), and neither total counts it.
func (r *regEnv) blankPropertyProblems(label string, g gridView, leaves []string, avgTotal bool) map[string][]string {
	out := map[string][]string{}
	if g.status != 200 {
		out[label+"grid read"] = []string{fmt.Sprintf("GET /api/grid -> %d: %s", g.status, clip(g.body, 300))}
		return out
	}
	wavg, wsum := r.metric["wavg"], r.metric["wsum"]
	pa, ps := []string{}, []string{}
	wsumTotal, wavgTotal := 0.0, 0.0
	for _, m := range months {
		for _, l := range leaves {
			where := l + " " + monthCode(m)
			if f, ok := regFactor[l]; ok {
				v, in := g.cell(wavg, l, monthCode(m))
				expectVal(&pa, where, v, in, f)
				v, in = g.cell(wsum, l, monthCode(m))
				expectVal(&ps, where, v, in, regRev(l, m)*f)
				continue
			}
			if v, in := g.cell(wavg, l, monthCode(m)); in {
				pa = append(pa, fmt.Sprintf("%s = %g, want no cell (blank)", where, v))
			}
			if v, in := g.cell(wsum, l, monthCode(m)); in && v != 0 {
				ps = append(ps, fmt.Sprintf("%s = %g, want no cell or 0", where, v))
			}
		}
		wsumTotal += wsumOf(leaves, m)
		wavgTotal += factorMean(leaves)
	}
	v, ok := g.totals[wsum]
	expectVal(&ps, "total", v, ok, wsumTotal)
	if avgTotal {
		v, ok := g.totals[wavg]
		expectVal(&pa, "total (FR left out)", v, ok, wavgTotal)
	}
	name := label + "wavg = geo.factor (agg average): FR no cell"
	if avgTotal {
		name += ", total excludes FR"
	}
	out[name] = pa
	out[label+"wsum = g_rev * geo.factor (agg sum): cells and total"] = ps
	return out
}

func (r *regEnv) checkBlankProperty() {
	all := regLeaves
	emea := leavesUnder("EMEA")
	res := waitFor("Regressions recalculation (developer, Props)", func() map[string][]string {
		return r.blankPropertyProblems("F1 developer grid: ", fetchGrid(r.dev, r.props, r.revA), all, true)
	})
	report(res)
	scope := map[string]string{r.regionDim: "EMEA"}
	report(r.blankPropertyProblems("F1 developer grid scoped to EMEA: ", fetchGridScoped(r.dev, r.props, r.revA, scope), emea, true))

	step("business_admin hides US of the Regressions revision from the business user (keeping the Working rule)")
	r.ba.call("PUT", "/api/business-admin/users/"+r.viewerUserID+"/access-rules", map[string]any{"rules": []map[string]string{
		{"rule_type": "dimension_member", "ref_id": r.main.member["US"], "access": "hidden"},
		{"rule_type": "dimension_member", "ref_id": r.member["US"], "access": "hidden"},
	}})
	visible := []string{"DE", "UK", "FR"}
	report(r.blankPropertyProblems("F1 viewer (US hidden) grid: ", fetchGrid(r.viewer, r.props, r.revA), visible, true))
	report(r.blankPropertyProblems("F1 viewer (US hidden) grid scoped to EMEA: ", fetchGridScoped(r.viewer, r.props, r.revA, scope), emea, true))
}

// ── F2 ──────────────────────────────────────────────────────────────────────

func (r *regEnv) checkPropertyChart() {
	const m = 2
	ctx := map[string]string{r.periodDim: monthCode(m)}
	for _, c := range []struct {
		label string
		codes []string
	}{
		{"leaves DE, UK, US exact; FR null", []string{"DE", "UK", "US"}},
		{"EMEA, AMER combine their leaves", []string{"EMEA", "AMER"}},
		// World is two levels up: its average is over the leaves (as the
		// scheduler's persisted total is), not a mean of EMEA's and AMER's.
		{"World combines its leaves (not a mean of child means)", []string{"World"}},
	} {
		exp := []chartExpect{}
		if c.codes[0] == "DE" {
			exp = append(exp, chartExpect{"wavg", "FR", nil})
		}
		for _, code := range c.codes {
			leaves := leavesUnder(code)
			exp = append(exp, chartExpect{"wavg", code, f(factorMean(leaves))}, chartExpect{"wsum", code, f(wsumOf(leaves, m))})
		}
		p := chartProblems(r.environment, r.dev, r.chartProps, ctx, exp, nil)
		check("F2 chart-data by geo @2026-02: wavg/wsum "+c.label, len(p) == 0, "%s", summarize(p))
	}
}

// ── F12 ─────────────────────────────────────────────────────────────────────

func (r *regEnv) checkDebugViews() {
	r.dev2ID, r.dev2 = r.newUser("dev2", "Devon Restricted", "developer")
	step("business_admin hides US of the Regressions revision from the second developer")
	r.ba.call("PUT", "/api/business-admin/users/"+r.dev2ID+"/access-rules", map[string]any{"rules": []map[string]string{
		{"rule_type": "dimension_member", "ref_id": r.member["US"], "access": "hidden"},
	}})

	combo, _ := json.Marshal(map[string]string{r.regionDim: "DE", r.periodDim: "2026-02"})
	path := "/api/developer/debug/calc?revision_id=" + r.revA + "&dim_members=" + url.QueryEscape(string(combo))
	raw, status := r.dev2.try("GET", path, nil)
	check("F12 debug/calc?dim_members= for a developer with a hidden member -> 403", status == 403, "GET %s -> %d %s", path, status, clip(raw, 200))

	raw, status = r.dev.try("GET", path, nil)
	p := []string{}
	if status != 200 {
		p = append(p, fmt.Sprintf("GET %s -> %d %s", path, status, clip(raw, 200)))
	} else {
		var rows []struct {
			MetricID string  `json:"metric_id"`
			Value    float64 `json:"value"`
		}
		_ = json.Unmarshal([]byte(raw), &rows)
		got := map[string]float64{}
		for _, row := range rows {
			got[row.MetricID] = row.Value
		}
		v, ok := got[r.metric["wsum"]]
		expectVal(&p, "wsum DE 2026-02", v, ok, regRev("DE", 2)*regFactor["DE"])
		v, ok = got[r.metric["wavg"]]
		expectVal(&p, "wavg DE 2026-02", v, ok, regFactor["DE"])
	}
	check("F12 debug/calc?dim_members= for the unrestricted developer -> 200 with the persisted values", len(p) == 0, "%s", summarize(p))

	usFacts := func(a *api) (us, visible int, err string) {
		raw, status := a.try("GET", "/api/developer/debug/facts?revision_id="+r.revA, nil)
		if status != 200 {
			return 0, 0, fmt.Sprintf("-> %d %s", status, clip(raw, 200))
		}
		var facts []struct {
			DimMembers string  `json:"dim_members"`
			RevisionID *string `json:"revision_id"`
		}
		if e := json.Unmarshal([]byte(raw), &facts); e != nil {
			return 0, 0, e.Error()
		}
		for _, fc := range facts {
			if fc.RevisionID == nil || *fc.RevisionID != r.revA {
				continue
			}
			dm := map[string]string{}
			_ = json.Unmarshal([]byte(fc.DimMembers), &dm)
			switch dm[r.regionDim] {
			case "US":
				us++
			case "DE", "UK", "FR":
				visible++
			}
		}
		return us, visible, ""
	}
	us, vis, errMsg := usFacts(r.dev)
	check("F12 debug/facts for the unrestricted developer lists US facts of the revision (control)", errMsg == "" && us > 0 && vis > 0,
		"us=%d visible=%d %s", us, vis, errMsg)
	us, vis, errMsg = usFacts(r.dev2)
	check("F12 debug/facts for a developer with US hidden lists no US fact", errMsg == "" && us == 0 && vis > 0,
		"us=%d visible=%d %s", us, vis, errMsg)
}

// ── F3 ──────────────────────────────────────────────────────────────────────

func (r *regEnv) checkPartialFormulaSource() {
	all := regLeaves
	res := waitFor("Summary recalculation", func() map[string][]string {
		g := fetchGrid(r.dev, r.summary, r.revA)
		if g.status != 200 {
			return map[string][]string{"F3 Summary grid read": {fmt.Sprintf("-> %d %s", g.status, clip(g.body, 300))}}
		}
		pw, pp := []string{}, []string{}
		for _, l := range []string{"DE", "FR", "US"} {
			v, ok := g.cell(r.metric["world_price"], l, "P1")
			expectVal(&pw, l+" P1", v, ok, cubeRatio(all))
			v, ok = g.cell(r.metric["plain_price"], l, "P1")
			expectVal(&pp, l+" P1", v, ok, cubeRatio([]string{l}))
		}
		return map[string][]string{
			`F3 grid: world_price = LOOKUP(price, geo, "World") over all channels, not a mean of ratios`: pw,
			"F3 grid: plain_price = price at [geo, product] = amount/units over channels":                pp,
		}
	})
	report(res)
	exp := []chartExpect{}
	for _, code := range []string{"EMEA", "AMER", "World"} {
		exp = append(exp, chartExpect{"plain_price", code, f(cubeRatio(leavesUnder(code)))},
			chartExpect{"world_price", code, f(cubeRatio(all))})
	}
	p := chartProblems(r.environment, r.dev, r.chartSummary, map[string]string{r.productDim: "P1"}, exp, nil)
	check("F3 chart-data by geo @P1: plain_price / world_price at parents are ratios of sums", len(p) == 0, "%s", summarize(p))
}

// ── F4 ──────────────────────────────────────────────────────────────────────

func (r *regEnv) checkEmptyPeriodsAtParent() {
	want := map[string]float64{"yvl": balLast(), "yva": balMean(), "fyl": balLast()}
	res := waitFor("Props time recalculation", func() map[string][]string {
		g := fetchGrid(r.dev, r.props, r.revA)
		out := map[string][]string{}
		for _, n := range []string{"yvl", "yva", "fyl"} {
			p := []string{}
			for _, m := range months {
				v, ok := g.cell(r.metric[n], "DE", monthCode(m))
				expectVal(&p, "DE "+monthCode(m), v, ok, want[n])
			}
			out["F4 grid: "+n+" at leaf DE, every month"] = p
		}
		return out
	})
	report(res)
	exp := []chartExpect{}
	for _, code := range []string{"DE", "EMEA", "World"} {
		for _, n := range []string{"yvl", "yva", "fyl"} {
			exp = append(exp, chartExpect{n, code, f(want[n])})
		}
	}
	p := chartProblems(r.environment, r.dev, r.chartProps, map[string]string{r.periodDim: "2026-05"}, exp, nil)
	check("F4 chart-data by geo @2026-05: YEARVALUE(bal last / avgp average) and LOOKUP(bal, month, FY26) at EMEA, World skip empty months",
		len(p) == 0, "%s", summarize(p))
}

// ── F5 ──────────────────────────────────────────────────────────────────────

func (r *regEnv) staffProblems(label string) map[string][]string {
	g := fetchGrid(r.dev, r.staff, r.revA)
	p := []string{}
	if g.status != 200 {
		p = append(p, fmt.Sprintf("GET /api/grid -> %d %s", g.status, clip(g.body, 300)))
	}
	for _, d := range []string{"D1", "D2", "D3"} {
		v, ok := g.cell(r.metric["north_sal"], d)
		expectVal(&p, d, v, ok, northSalary())
	}
	return map[string][]string{label: p}
}

func (r *regEnv) checkRelatedDimensionRecalc() {
	report(waitFor("Staff recalculation", func() map[string][]string {
		return r.staffProblems(`F5 grid: north_sal = SUMIFS(salary, area, "North") (dept's parent dimension is area)`)
	}))
	step("moving D2 from South to North (PATCH member parent)")
	r.dev.call("PATCH", "/api/developer/dimensions/"+r.deptDim+"/members/"+r.member["D2"], map[string]any{
		"code": "D2", "label": "Department D2", "parent_member_id": r.member["North"],
	})
	deptArea["D2"] = "North"
	report(waitFor("recalculation after the move", func() map[string][]string {
		return r.staffProblems("F5 grid: north_sal recalculated after D2 moved to North")
	}))

	// The same through a property grouping created over HTTP.
	report(waitFor("People/Teams recalculation", func() map[string][]string {
		return r.groupingProblems(`F5 grouping: north_pay = SUMIFS(pay, team, "North") and Teams grid team_pay = pay by team (team groups emp by its team property)`)
	}))
	step("moving P2 from team South to North (PATCH member properties)")
	empTeam["P2"] = "North"
	r.setEmpTeam("P2")
	report(waitFor("recalculation after the property edit", func() map[string][]string {
		return r.groupingProblems("F5 grouping: north_pay and the Teams grid recalculated after P2's team became North")
	}))
}

// setEmpTeam writes an employee's team property (PATCH merges it).
func (r *regEnv) setEmpTeam(p string) {
	r.dev.call("PATCH", "/api/developer/dimensions/"+r.empDim+"/members/"+r.member[p], map[string]any{
		"code": p, "label": "Employee " + p, "properties": map[string]string{"team": empTeam[p]},
	})
}

// groupingProblems compares north_pay on People and team_pay on Teams with
// the pay summed by the employees' current team property.
func (r *regEnv) groupingProblems(label string) map[string][]string {
	p := []string{}
	people := fetchGrid(r.dev, r.people, r.revA)
	if people.status != 200 {
		p = append(p, fmt.Sprintf("GET /api/grid People -> %d %s", people.status, clip(people.body, 300)))
	}
	for _, e := range []string{"P1", "P2", "P3"} {
		v, ok := people.cell(r.metric["north_pay"], e)
		expectVal(&p, "north_pay@"+e, v, ok, teamPay("North"))
	}
	teams := fetchGrid(r.dev, r.teams, r.revA)
	if teams.status != 200 {
		p = append(p, fmt.Sprintf("GET /api/grid Teams -> %d %s", teams.status, clip(teams.body, 300)))
	}
	for _, t := range []string{"North", "South"} {
		v, ok := teams.cell(r.metric["team_pay"], t)
		expectVal(&p, "team_pay@"+t, v, ok, teamPay(t))
	}
	return map[string][]string{label: p}
}

// ── F10 ─────────────────────────────────────────────────────────────────────

func (r *regEnv) checkDimensionInUse() {
	path := "/api/developer/dimensions/" + r.tierDim
	raw, status := r.dev.try("DELETE", path, nil)
	check("F10 deleting dimension tier while tier_n's formula names it -> 409 DIMENSION_IN_USE naming tier_n",
		status == 409 && strings.Contains(raw, "DIMENSION_IN_USE") && strings.Contains(raw, "tier_n"), "DELETE %s -> %d %s", path, status, clip(raw, 300))
	found := false
	for _, d := range r.dev.list("GET", "/api/developer/dimensions?revision_id="+r.revA, nil) {
		if d["id"] == r.tierDim {
			found = true
		}
	}
	check("F10 the refused delete kept dimension tier", found, "")
	r.dev.call("DELETE", "/api/developer/metrics/"+r.metric["tier_n"], nil)
	raw, status = r.dev.try("DELETE", path, nil)
	check("F10 once tier_n is deleted, deleting tier succeeds (control)", status == 200 || status == 204, "DELETE %s -> %d %s", path, status, clip(raw, 300))
}

// ── F6 / F8 ─────────────────────────────────────────────────────────────────

// zonesProblems reads the Zones grid. Before the formula changes, lk_z2 =
// LOOKUP(sales, zone, "Z2") serves Z2's sales at every zone; after it is
// changed to LOOKUP(sales, zone, "Z1") and Z2 is deleted, it serves Z1's
// sales at Z1 only, and nothing is served at Z2.
func (r *regEnv) zonesProblems(moved bool) map[string][]string {
	g := fetchGrid(r.dev, r.zones, r.revA)
	if g.status != 200 {
		return map[string][]string{"F6 Zones grid read": {fmt.Sprintf("-> %d %s", g.status, clip(g.body, 300))}}
	}
	lk := r.metric["lk_z2"]
	p := []string{}
	v, ok := g.cell(r.metric["sales"], "Z1")
	expectVal(&p, "sales Z1", v, ok, zoneSales["Z1"])
	if !moved {
		for z := range zoneSales {
			v, ok := g.cell(lk, z)
			expectVal(&p, "lk_z2 "+z, v, ok, zoneSales["Z2"])
		}
		v, ok := g.totals[lk]
		expectVal(&p, "lk_z2 total", v, ok, zoneSales["Z2"]*float64(len(zoneSales)))
		return map[string][]string{`F6 grid: lk_z2 = LOOKUP(sales, zone, "Z2") serves Z2's sales at every zone`: p}
	}
	v, ok = g.cell(lk, "Z1")
	expectVal(&p, "lk_z2 Z1", v, ok, zoneSales["Z1"])
	v, ok = g.totals[lk]
	expectVal(&p, "lk_z2 total", v, ok, zoneSales["Z1"])
	for _, m := range []string{"sales", "lk_z2"} {
		if v, ok := g.cell(r.metric[m], "Z2"); ok {
			p = append(p, fmt.Sprintf("%s cell Z2 = %g still served after Z2 was deleted", m, v))
		}
	}
	return map[string][]string{`F6 grid: after lk_z2 = LOOKUP(sales, zone, "Z1") and Z2's delete, Z1's sales at Z1 only`: p}
}

// checkDeletedLookupMember: deleting a member a formula names is refused
// (MEMBER_IN_USE, like DIMENSION_IN_USE and PROPERTY_IN_USE) and the cells
// keep being served; once the formula no longer names it, the delete goes
// through. A member RENAME is not refused and does not rewrite formulas,
// so it is what still reaches activation's UNKNOWN_MEMBER (F8).
func (r *regEnv) checkDeletedLookupMember() {
	report(waitFor("Zones recalculation", func() map[string][]string { return r.zonesProblems(false) }))
	path := "/api/developer/dimensions/" + r.zoneDim + "/members/" + r.member["Z2"]
	raw, status := r.dev.try("DELETE", path, nil)
	check("F6 deleting member Z2 while lk_z2's literal LOOKUP names it -> 409 MEMBER_IN_USE naming lk_z2",
		status == 409 && strings.Contains(raw, "MEMBER_IN_USE") && strings.Contains(raw, "lk_z2"), "DELETE %s -> %d %s", path, status, clip(raw, 300))
	report(r.zonesProblems(false)) // the refused delete kept Z2 and every served cell

	// A member code the dimension already has is a 409 MEMBER_CODE_TAKEN on
	// create and on a recode (it was a 500 carrying the SQL text).
	members := "/api/developer/dimensions/" + r.zoneDim + "/members"
	raw, status = r.dev.try("POST", members, map[string]any{"code": "Z1", "label": "Zone 1 again"})
	check("F6 adding a second member Z1 to zone -> 409 MEMBER_CODE_TAKEN",
		status == 409 && strings.Contains(raw, "MEMBER_CODE_TAKEN") && !strings.Contains(raw, "SQLSTATE"), "POST %s -> %d %s", members, status, clip(raw, 300))
	raw, status = r.dev.try("PATCH", members+"/"+r.member["Z1"], map[string]any{"code": "Z2", "label": "Zone 1"})
	check("F6 recoding Z1 to the taken code Z2 -> 409 MEMBER_CODE_TAKEN",
		status == 409 && strings.Contains(raw, "MEMBER_CODE_TAKEN") && !strings.Contains(raw, "SQLSTATE"), "PATCH -> %d %s", status, clip(raw, 300))

	// A PATCH carrying only the formula is a partial update (it once wrote
	// the name as "" and answered 500 on a revision's second such PATCH).
	raw, status = r.dev.try("PATCH", "/api/developer/metrics/"+r.metric["lk_z2"], map[string]any{"formula": `LOOKUP(sales, zone, "Z1")`})
	check("F6 PATCH lk_z2 with only a formula -> 200", status == 200, "got %d %s", status, clip(raw, 300))
	raw, status = r.dev.try("DELETE", path, nil)
	check("F6 once lk_z2 names Z1 instead, deleting Z2 succeeds", status == 200 || status == 204, "DELETE %s -> %d %s", path, status, clip(raw, 300))
	report(waitFor("recalculation after the formula change and the member delete", func() map[string][]string { return r.zonesProblems(true) }))

	// F8: a member rename leaves lk_z2 naming the old code Z1.
	r.dev.call("PATCH", "/api/developer/dimensions/"+r.zoneDim+"/members/"+r.member["Z1"], map[string]any{"code": "Z1x", "label": "Zone 1"})
	if got := r.formulaOf(r.dev, "lk_z2"); !strings.Contains(got, `"Z1"`) {
		check("F8 lk_z2 still names Z1 after the rename (formulas are not rewritten on a member rename)", false, "formula now %q", got)
		return
	}
	raw, status = r.dev.try("PUT", "/api/developer/revisions/"+r.revA+"/activate", nil)
	check("F8 activating the revision while lk_z2 names the renamed-away member Z1 -> 400 UNKNOWN_MEMBER naming lk_z2",
		status == 400 && strings.Contains(raw, "UNKNOWN_MEMBER") && strings.Contains(raw, "lk_z2"), "got %d %s", status, clip(raw, 300))
}
