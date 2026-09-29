package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// environment is everything the checks need to address what was built.
type environment struct {
	root                  *api // platform_admin
	dev                   *api
	tenantID, workspaceID string
	appID, modelID        string
	rev                   string

	regionDim, currencyDim, periodDim, dayDim string
	member                                    map[string]string // dimension id + "/" + code -> member id
	planGrid, fxGrid, dailyGrid               string
	metric                                    map[string]string // name -> id
}

func bootstrap(root *api) *environment {
	admin := root.as(*flagAdmin)
	if _, status := admin.try("GET", "/api/admin/tenants", nil); status != 200 {
		fatalf("persona %q is not a platform administrator on %s (GET /api/admin/tenants -> %d)", *flagAdmin, root.base, status)
	}
	for _, t := range admin.list("GET", "/api/admin/tenants", nil) {
		if name, _ := t["name"].(string); name == tenantName {
			step("deleting stale tenant %q from an earlier run (%s)", tenantName, id(t))
			admin.call("DELETE", "/api/admin/tenants/"+id(t), nil)
		}
	}
	e := &environment{root: admin, member: map[string]string{}, metric: map[string]string{}}

	step("creating tenant %q", tenantName)
	t := admin.call("POST", "/api/admin/tenants", map[string]any{"name": tenantName, "plan": "enterprise"})
	e.tenantID = id(t)
	e.workspaceID, _ = t["workspace_id"].(string)
	if e.workspaceID == "" {
		fatalf("tenant creation returned no workspace_id: %v", t)
	}
	app := admin.call("POST", "/api/admin/applications", map[string]any{
		"customer_id": e.tenantID, "name": "Formula Catalogue", "mode": "planning",
	})
	e.appID = id(app)
	admin = admin.withApp(e.appID)
	e.root = admin
	e.modelID = id(admin.call("POST", "/api/admin/models", map[string]any{"application_id": e.appID, "name": "Formula Catalogue"}))

	email := fmt.Sprintf("dev.%s@formula-catalogue.test", strconv.FormatInt(time.Now().UnixNano(), 36))
	u := admin.call("POST", "/api/admin/users", map[string]any{"email": email, "display_name": "Dana Developer"})
	admin.call("POST", "/api/admin/users/"+id(u)+"/roles", map[string]any{"role": "developer", "workspace_id": e.workspaceID})
	e.dev = admin.as("admin-created-" + email)
	step("user %s (developer)", email)

	step("creating and activating revision \"Working\"")
	e.rev = id(e.dev.call("POST", "/api/developer/revisions", map[string]any{"name": "Working"}))
	e.dev.call("PUT", "/api/developer/revisions/"+e.rev+"/activate", nil)
	return e
}

func (e *environment) dim(name string, extra map[string]any) string {
	body := map[string]any{"name": name, "agg_rule": "sum", "revision_id": e.rev}
	for k, v := range extra {
		body[k] = v
	}
	return id(e.dev.call("POST", "/api/developer/dimensions", body))
}

func (e *environment) addMember(dimID, code, parentCode string, props map[string]string) {
	body := map[string]any{"code": code, "label": code}
	if parentCode != "" {
		body["parent_member_id"] = e.member[dimID+"/"+parentCode]
	}
	mid := id(e.dev.call("POST", "/api/developer/dimensions/"+dimID+"/members", body))
	e.member[dimID+"/"+code] = mid
	if len(props) > 0 {
		body["properties"] = props
		e.dev.call("PATCH", "/api/developer/dimensions/"+dimID+"/members/"+mid, body)
	}
}

func (e *environment) grid(name string, dims ...string) string {
	g := id(e.dev.call("POST", "/api/developer/grids", map[string]any{"name": name, "revision_id": e.rev}))
	for _, d := range dims {
		e.dev.call("POST", "/api/developer/grids/"+g+"/dimensions/"+d, nil)
	}
	return g
}

func (e *environment) input(name, gridID, summary string) {
	body := map[string]any{"name": name, "is_input": true, "revision_id": e.rev}
	if summary != "" {
		body["time_summary"] = summary
	}
	e.metric[name] = id(e.dev.call("POST", "/api/developer/metrics", body))
	e.dev.call("POST", "/api/developer/grids/"+gridID+"/metrics/"+e.metric[name], nil)
}

func buildModel(e *environment) {
	step("dimension region (World > EMEA > DE, UK; World > AMER > US) with segment, factor, opened, currency")
	e.regionDim = e.dim("region", nil)
	for _, p := range []struct{ name, typ string }{{"segment", "text"}, {"factor", "number"}, {"opened", "date"}, {"currency", "text"}} {
		e.dev.call("POST", "/api/developer/dimensions/"+e.regionDim+"/properties", map[string]any{"name": p.name, "data_type": p.typ})
	}
	e.addMember(e.regionDim, "World", "", nil)
	e.addMember(e.regionDim, "EMEA", "World", map[string]string{"factor": "3"})
	e.addMember(e.regionDim, "AMER", "World", nil)
	for _, r := range leafRegions {
		p := props[r]
		e.addMember(e.regionDim, r, p.parent, map[string]string{
			"segment": p.segment, "factor": strconv.FormatFloat(p.factor, 'f', -1, 64),
			"opened": p.opened.Format("2006-01-02"), "currency": p.currency,
		})
	}

	step("dimension currency (EUR, GBP, USD)")
	e.currencyDim = e.dim("currency", nil)
	for _, c := range []string{"EUR", "GBP", "USD"} {
		e.addMember(e.currencyDim, c, "", nil)
	}

	step("time dimension period (monthly, fiscal year from January): FY26 > H1/H2 > Q1..Q4 > months")
	e.periodDim = e.dim("period", map[string]any{"dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1})
	e.addMember(e.periodDim, "FY26", "", nil)
	e.addMember(e.periodDim, "H1", "FY26", nil)
	e.addMember(e.periodDim, "H2", "FY26", nil)
	for q := 1; q <= 4; q++ {
		h := "H1"
		if q > 2 {
			h = "H2"
		}
		qc := fmt.Sprintf("Q%d", q)
		e.addMember(e.periodDim, qc, h, nil)
		first := (q-1)*3 + 1
		e.dev.call("POST", "/api/developer/dimensions/"+e.periodDim+"/members/generate", map[string]any{
			"start": fmt.Sprintf("2026-%02d-01", first), "end": fmt.Sprintf("2026-%02d-%02d", first+2, daysIn(first+2)),
			"parent_member_id": e.member[e.periodDim+"/"+qc],
		})
	}
	codes := e.memberCodes(e.periodDim)
	for m := 1; m <= 12; m++ {
		if !codes[monthCode(m)] {
			fatalf("generated periods do not include %s (have %v)", monthCode(m), codes)
		}
	}

	step("time dimension day (daily, 2026-01-01 .. 2026-02-28)")
	e.dayDim = e.dim("day", map[string]any{"dimension_type": "time", "time_granularity": "day", "fiscal_year_start_month": 1})
	e.dev.call("POST", "/api/developer/dimensions/"+e.dayDim+"/members/generate", map[string]any{"start": "2026-01-01", "end": "2026-02-28"})
	dcodes := e.memberCodes(e.dayDim)
	for k := 1; k <= days; k++ {
		if !dcodes[dayCode(k)] {
			fatalf("generated days do not include %s (have %d codes)", dayCode(k), len(dcodes))
		}
	}

	step("grids Plan [region, period], FX [currency], Daily [day] and their inputs")
	e.planGrid = e.grid("Plan", e.regionDim, e.periodDim)
	e.fxGrid = e.grid("FX", e.currencyDim)
	e.dailyGrid = e.grid("Daily", e.dayDim)
	for _, in := range []struct{ name, grid, summary string }{
		{"sales", e.planGrid, "sum"}, {"cost", e.planGrid, "sum"}, {"units", e.planGrid, "sum"},
		{"lag_n", e.planGrid, "last"}, {"flow", e.planGrid, "sum"},
		{"fx_rate", e.fxGrid, ""}, {"daily_sales", e.dailyGrid, "sum"},
	} {
		e.input(in.name, in.grid, in.summary)
	}

	defs := catalogue()
	step("%d calculated metrics", len(defs))
	for _, d := range defs {
		formula := d.formula
		if d.first != "" {
			formula = d.first
		}
		body := map[string]any{"name": d.name, "is_input": false, "formula": "=" + formula, "agg_rule": d.aggRule(), "revision_id": e.rev}
		if d.summary != "" {
			body["time_summary"] = d.summary
		}
		raw, status := e.dev.try("POST", "/api/developer/metrics", body)
		if status != 200 {
			fatalf("create calculated metric %s = %s -> %d: %s", d.name, d.formula, status, clip(raw, 600))
		}
		e.metric[d.name] = idFromRaw(raw)
		gridID := e.planGrid
		if d.daily {
			gridID = e.dailyGrid
		}
		if raw, status := e.dev.try("POST", "/api/developer/grids/"+gridID+"/metrics/"+e.metric[d.name], nil); status != 200 {
			fatalf("place %s on its grid -> %d: %s", d.name, status, clip(raw, 600))
		}
	}
	for _, d := range defs {
		if d.first == "" {
			continue
		}
		step("closing the recurrence: %s = %s", d.name, d.formula)
		if raw, status := e.dev.try("PATCH", "/api/developer/metrics/"+e.metric[d.name], map[string]any{"formula": "=" + d.formula}); status != 200 {
			fatalf("update %s = %s -> %d: %s", d.name, d.formula, status, clip(raw, 600))
		}
	}
}

// memberCodes lists a dimension's member codes from the developer's listing.
func (e *environment) memberCodes(dimID string) map[string]bool {
	codes := map[string]bool{}
	for _, d := range e.dev.list("GET", "/api/developer/dimensions?revision_id="+e.rev, nil) {
		if d["id"] != dimID {
			continue
		}
		ms, _ := d["members"].([]any)
		for _, m := range ms {
			if mm, ok := m.(map[string]any); ok {
				if c, _ := mm["code"].(string); c != "" {
					codes[c] = true
				}
			}
		}
	}
	return codes
}

func (e *environment) putFact(metric string, dims map[string]string, v float64) {
	e.dev.call("POST", "/api/cells", map[string]any{
		"model_id": e.modelID, "metric_id": e.metric[metric], "revision_id": e.rev, "dim_codes": dims, "value": v,
	})
}

func writeFacts(e *environment) {
	step("writing sales, cost, units, lag_n and flow for 3 regions x 12 months, fx_rate for 3 currencies, daily_sales for %d days", days)
	for _, r := range leafRegions {
		for m := 1; m <= 12; m++ {
			at := map[string]string{e.regionDim: r, e.periodDim: monthCode(m)}
			e.putFact("sales", at, sales(r, m))
			e.putFact("cost", at, cost(r, m))
			e.putFact("units", at, units(r, m))
			if v := lagN(r, m); v != 0 {
				e.putFact("lag_n", at, v)
			}
			e.putFact("flow", at, flow(r, m))
		}
	}
	for c, v := range fxRates {
		e.putFact("fx_rate", map[string]string{e.currencyDim: c}, v)
	}
	for k := 1; k <= days; k++ {
		e.putFact("daily_sales", map[string]string{e.dayDim: dayCode(k)}, float64(k))
	}
	// Activation recalculates every calculated metric of the revision,
	// including the ones that read no input (TODAY, START, COUNTIFS).
	step("re-activating the revision to recalculate it as a whole")
	e.dev.call("PUT", "/api/developer/revisions/"+e.rev+"/activate", nil)
}

// ── grid reads ─────────────────────────────────────────────────────────────

type gridView struct {
	cells  map[string]float64 // metricID#sorted codes
	totals map[string]float64
	status int
	body   string
}

func normKey(metricID string, codes ...string) string {
	c := append([]string(nil), codes...)
	sort.Strings(c)
	return metricID + "#" + strings.Join(c, "|")
}

func fetchGrid(a *api, gridID, revisionID string) gridView {
	q := url.Values{}
	q.Set("grid_def_id", gridID)
	q.Set("revision_id", revisionID)
	raw, status := a.raw("GET", "/api/grid?"+q.Encode(), nil)
	g := gridView{cells: map[string]float64{}, totals: map[string]float64{}, status: status, body: string(raw)}
	if status != 200 {
		return g
	}
	var resp struct {
		Cells  map[string]float64 `json:"cells"`
		Totals map[string]float64 `json:"totals"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		g.status = -1
		return g
	}
	for k, v := range resp.Cells {
		parts := strings.Split(k, ":")
		g.cells[normKey(parts[0], parts[1:]...)] = v
	}
	for k, v := range resp.Totals {
		g.totals[k] = v
	}
	return g
}

func (g gridView) cell(metricID string, codes ...string) (float64, bool) {
	v, ok := g.cells[normKey(metricID, codes...)]
	return v, ok
}

func approx(got, want float64) bool {
	d := math.Abs(got - want)
	return d <= 1e-6 || d <= 1e-9*math.Max(math.Abs(got), math.Abs(want))
}
