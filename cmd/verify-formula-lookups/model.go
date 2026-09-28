package main

import (
	"fmt"
	"strconv"
	"time"
)

// environment is everything the checks need to address what was built.
type environment struct {
	root                   *api // platform_admin
	dev, ba, viewer        *api
	tenantID, workspaceID  string
	appID, modelID         string
	baUserID, viewerUserID string
	revA                   string // the revision everything is built in

	regionDim, currencyDim, periodDim string
	member                            map[string]string // code -> member id (region, currency, period)
	propID                            map[string]string // property name -> id (region)
	props                             map[string]*regionProps

	planGrid, fxGrid string
	metric           map[string]string // name -> id
	defs             []calcDef

	dashboard                              string
	chartRegion, chartMonths, chartSegment string
}

// bootstrap builds the tenant, application, model, users and the working
// revision, as platform_admin and then as the developer.
func bootstrap(root *api) *environment {
	admin := root.as(*flagAdmin)
	if _, status := admin.try("GET", "/api/admin/tenants", nil); status != 200 {
		fatalf("persona %q is not a platform administrator on %s (GET /api/admin/tenants -> %d); "+
			"the database needs its first platform admin (scripts/bootstrap-platform-admin.sh)", *flagAdmin, root.base, status)
	}
	for _, t := range admin.list("GET", "/api/admin/tenants", nil) {
		if name, _ := t["name"].(string); name == tenantName {
			step("deleting stale tenant %q from an earlier run (%s)", tenantName, id(t))
			admin.call("DELETE", "/api/admin/tenants/"+id(t), nil)
		}
	}
	e := &environment{root: admin, member: map[string]string{}, propID: map[string]string{}, metric: map[string]string{}}

	step("creating tenant %q", tenantName)
	t := admin.call("POST", "/api/admin/tenants", map[string]any{"name": tenantName, "plan": "enterprise"})
	e.tenantID = id(t)
	e.workspaceID, _ = t["workspace_id"].(string)
	if e.workspaceID == "" {
		fatalf("tenant creation returned no workspace_id: %v", t)
	}
	app := admin.call("POST", "/api/admin/applications", map[string]any{
		"customer_id": e.tenantID, "name": "Formula Lookups", "mode": "planning",
	})
	e.appID = id(app)
	admin = admin.withApp(e.appID)
	e.root = admin
	e.modelID = id(admin.call("POST", "/api/admin/models", map[string]any{"application_id": e.appID, "name": "Formula Lookups"}))

	_, e.dev = e.newUser("dev", "Dana Developer", "developer")
	e.baUserID, e.ba = e.newUser("ba", "Bea Admin", "business_admin")
	e.viewerUserID, e.viewer = e.newUser("viewer", "Vic Viewer", "business_user")

	step("creating and activating revision \"Working\"")
	e.revA = id(e.dev.call("POST", "/api/developer/revisions", map[string]any{"name": "Working"}))
	e.dev.call("PUT", "/api/developer/revisions/"+e.revA+"/activate", nil)
	return e
}

// newUser creates a user with one workspace role, as platform_admin, and
// returns its id and a client acting as it. A per-run suffix keeps the
// addresses unique even if a tenant delete ever leaves its users behind.
func (e *environment) newUser(local, name, role string) (string, *api) {
	email := fmt.Sprintf("%s.%s@formula-lookups.test", local, strconv.FormatInt(time.Now().UnixNano(), 36))
	u := e.root.call("POST", "/api/admin/users", map[string]any{"email": email, "display_name": name})
	e.root.call("POST", "/api/admin/users/"+id(u)+"/roles", map[string]any{"role": role, "workspace_id": e.workspaceID})
	step("user %s (%s)", email, role)
	return id(u), e.root.as("admin-created-" + email)
}

func (e *environment) dim(name string, extra map[string]any) string {
	body := map[string]any{"name": name, "agg_rule": "sum", "revision_id": e.revA}
	for k, v := range extra {
		body[k] = v
	}
	return id(e.dev.call("POST", "/api/developer/dimensions", body))
}

func (e *environment) addMember(dimID, code, label, parentCode string, extra map[string]any) string {
	body := map[string]any{"code": code, "label": label}
	if parentCode != "" {
		body["parent_member_id"] = e.member[parentCode]
	}
	for k, v := range extra {
		body[k] = v
	}
	mid := id(e.dev.call("POST", "/api/developer/dimensions/"+dimID+"/members", body))
	e.member[code] = mid
	return mid
}

// setMemberProps writes a region member's property values (PATCH merges them
// over the member's existing properties).
func (e *environment) setMemberProps(code, label, parentCode string, props map[string]string) {
	body := map[string]any{"code": code, "label": label, "properties": props}
	if parentCode != "" {
		body["parent_member_id"] = e.member[parentCode]
	}
	e.dev.call("PATCH", "/api/developer/dimensions/"+e.regionDim+"/members/"+e.member[code], body)
}

var regionTree = []struct{ code, label, parent string }{
	{"World", "World", ""},
	{"EMEA", "EMEA", "World"},
	{"AMER", "Americas", "World"},
	{"DE", "Germany", "EMEA"},
	{"UK", "United Kingdom", "EMEA"},
	{"US", "United States", "AMER"},
}

func buildModel(e *environment) {
	e.props = initialRegionProps()

	step("dimension region (World > EMEA > DE, UK; World > AMER > US) with properties segment/factor/currency")
	e.regionDim = e.dim("region", nil)
	for _, m := range regionTree {
		e.addMember(e.regionDim, m.code, m.label, m.parent, nil)
	}
	for _, p := range []struct{ name, typ string }{{"segment", "text"}, {"factor", "number"}, {"currency", "text"}} {
		e.propID[p.name] = id(e.dev.call("POST", "/api/developer/dimensions/"+e.regionDim+"/properties",
			map[string]any{"name": p.name, "data_type": p.typ}))
	}
	for _, m := range regionTree {
		p := e.props[m.code]
		vals := map[string]string{}
		if p.segment != "" {
			vals["segment"] = p.segment
		}
		if p.hasFac {
			vals["factor"] = strconv.FormatFloat(p.factor, 'f', -1, 64)
		}
		if p.currency != "" {
			vals["currency"] = p.currency
		}
		if len(vals) > 0 {
			e.setMemberProps(m.code, m.label, m.parent, vals)
		}
	}

	step("dimension currency (EUR, GBP, USD)")
	e.currencyDim = e.dim("currency", nil)
	for _, c := range []string{"EUR", "GBP", "USD"} {
		e.addMember(e.currencyDim, c, c, "", nil)
	}

	e.buildPeriod("period")

	step("grids Plan [region, period] and FX [currency]")
	e.planGrid = id(e.dev.call("POST", "/api/developer/grids", map[string]any{"name": "Plan", "revision_id": e.revA}))
	e.dev.call("POST", "/api/developer/grids/"+e.planGrid+"/dimensions/"+e.regionDim, nil)
	e.dev.call("POST", "/api/developer/grids/"+e.planGrid+"/dimensions/"+e.periodDim, nil)
	e.fxGrid = id(e.dev.call("POST", "/api/developer/grids", map[string]any{"name": "FX", "revision_id": e.revA}))
	e.dev.call("POST", "/api/developer/grids/"+e.fxGrid+"/dimensions/"+e.currencyDim, nil)

	step("input metrics revenue, lag_n [region, period] and fx_rate [currency]")
	for _, in := range []struct{ name, grid, summary string }{
		{"revenue", e.planGrid, "sum"}, {"lag_n", e.planGrid, "last"}, {"fx_rate", e.fxGrid, ""},
	} {
		body := map[string]any{"name": in.name, "is_input": true, "revision_id": e.revA}
		if in.summary != "" {
			body["time_summary"] = in.summary
		}
		e.metric[in.name] = id(e.dev.call("POST", "/api/developer/metrics", body))
		e.dev.call("POST", "/api/developer/grids/"+in.grid+"/metrics/"+e.metric[in.name], nil)
	}

	e.defs = calcDefs()
	step("%d calculated metrics on Plan", len(e.defs))
	for _, d := range e.defs {
		agg := d.agg
		if agg == "" {
			agg = "sum"
		}
		raw, status := e.dev.try("POST", "/api/developer/metrics", map[string]any{
			"name": d.name, "is_input": false, "formula": "=" + d.formula, "agg_rule": agg, "revision_id": e.revA,
		})
		if status != 200 {
			fatalf("create calculated metric %s = %s -> %d: %s", d.name, d.formula, status, clip(raw, 600))
		}
		e.metric[d.name] = idFromRaw(raw)
		if raw, status := e.dev.try("POST", "/api/developer/grids/"+e.planGrid+"/metrics/"+e.metric[d.name], nil); status != 200 {
			fatalf("place %s on Plan -> %d: %s", d.name, status, clip(raw, 600))
		}
	}

	step("dashboard with three charts on Plan")
	e.dashboard = id(e.dev.call("POST", "/api/developer/dashboards", map[string]any{"name": "Lookups", "revision_id": e.revA}))
	e.chartRegion = e.addChart("bar", e.regionDim, []string{"revenue", "share_world", "rev_usd", "fx_region", "sum_de_code"},
		map[string]string{e.periodDim: "2026-03"}, 0)
	e.chartMonths = e.addChart("line", e.periodDim, []string{"qv", "hytd", "yv", "ts_q2", "lag_rev"},
		map[string]string{e.regionDim: "DE"}, 1)
	e.chartSegment = e.addChart("bar", e.regionDim, []string{"ent_rev", "same_seg", "cnt_ent", "max_big", "sumif_smb"},
		map[string]string{e.periodDim: "2026-05"}, 2)
}

// buildPeriod creates a monthly time dimension (fiscal year from January)
// with FY26 > H1/H2 > Q1..Q4 > months in the environment's revision.
func (e *environment) buildPeriod(name string) {
	step("time dimension %s (monthly, fiscal year from January) with FY26 > H1/H2 > Q1..Q4 > months", name)
	e.periodDim = e.dim(name, map[string]any{"dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1})
	e.addMember(e.periodDim, "FY26", "FY 2026", "", nil)
	e.addMember(e.periodDim, "H1", "H1 2026", "FY26", nil)
	e.addMember(e.periodDim, "H2", "H2 2026", "FY26", nil)
	for q := 1; q <= 4; q++ {
		h := "H1"
		if q > 2 {
			h = "H2"
		}
		qc := fmt.Sprintf("Q%d", q)
		e.addMember(e.periodDim, qc, qc+" 2026", h, nil)
		first := (q-1)*3 + 1
		last := first + 2
		e.dev.call("POST", "/api/developer/dimensions/"+e.periodDim+"/members/generate", map[string]any{
			"start": fmt.Sprintf("2026-%02d-01", first), "end": fmt.Sprintf("2026-%02d-%02d", last, daysIn2026[last-1]),
			"parent_member_id": e.member[qc],
		})
	}
	// The generator names the periods; confirm it used the codes the
	// formulas and facts below rely on.
	codes := e.memberCodes(e.periodDim)
	for _, m := range months {
		if !codes[monthCode(m)] {
			fatalf("generated periods do not include %s (have %v)", monthCode(m), codes)
		}
	}
}

func (e *environment) addChart(kind, dimID string, metricNames []string, ctx map[string]string, row int) string {
	return e.addGridChart(e.planGrid, kind, dimID, metricNames, ctx, row)
}

func (e *environment) addGridChart(gridID, kind, dimID string, metricNames []string, ctx map[string]string, row int) string {
	ids := make([]string, 0, len(metricNames))
	for _, n := range metricNames {
		ids = append(ids, e.metric[n])
	}
	w := e.dev.call("POST", "/api/developer/dashboards/"+e.dashboard+"/widgets", map[string]any{
		"widget_type": "chart", "ref_id": gridID,
		"pos_x": 0, "pos_y": row * 4, "size_w": 12, "size_h": 4,
		"widget_props": map[string]any{"chart": map[string]any{
			"chart_type": kind, "dimension_id": dimID, "metric_ids": ids, "context_defaults": ctx,
		}},
	})
	return id(w)
}

// memberCodes lists a dimension's member codes from the developer's
// dimension listing.
func (e *environment) memberCodes(dimID string) map[string]bool {
	codes := map[string]bool{}
	for _, d := range e.dev.list("GET", "/api/developer/dimensions?revision_id="+e.revA, nil) {
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

// putFact writes one input value through POST /api/cells.
func (e *environment) putFact(metric string, dims map[string]string, v float64) {
	e.dev.call("POST", "/api/cells", map[string]any{
		"model_id": e.modelID, "metric_id": e.metric[metric], "revision_id": e.revA, "dim_codes": dims, "value": v,
	})
}

func writeFacts(e *environment) {
	step("writing revenue and lag_n for 3 regions x 12 months, fx_rate for 3 currencies")
	put := e.putFact
	for _, r := range leafRegions {
		for _, m := range months {
			put("revenue", map[string]string{e.regionDim: r, e.periodDim: monthCode(m)}, rev(r, m))
			put("lag_n", map[string]string{e.regionDim: r, e.periodDim: monthCode(m)}, lagN(r, m))
		}
	}
	for c, v := range fxRates {
		put("fx_rate", map[string]string{e.currencyDim: c}, v)
	}
	// Activation recalculates every calculated metric of the revision —
	// including the ones that read no metric (COUNTIFS, START/END,
	// DAYSINMONTH), which no fact write reaches through the dependency graph.
	step("re-activating the revision to recalculate it as a whole")
	e.dev.call("PUT", "/api/developer/revisions/"+e.revA+"/activate", nil)
}
