// Restricted viewers and scoped reads of calculated metrics over HTTP
// (FORMULA_CALCULATION_INSTRUCTIONS.md, contracts C6 and C7): a developer
// builds a model with LOOKUP, SUMIFS, a member-local FX LOOKUP, time
// windows (PREVIOUS, a dynamic LAG, YEARVALUE, TIMESUM), a coarse-grained
// metric, a dim.property metric and an ordinary metric reading a served
// one; a business admin hides members from a business user through the
// real access-rules endpoint; and the business user's grid, chart-data and
// metric list withhold exactly the values whose read set touches a hidden
// member — every other value served exactly as persisted.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type restrictedFixture struct {
	t                     *testing.T
	pool                  *pgxpool.Pool
	srv                   *httptest.Server
	appID, modelID, revID string
	dev, admin, viewer    string
	viewerID              string

	region, currency, period string            // dimension IDs
	members                  map[string]string // member code -> member ID (codes are unique across dimensions here)
	metric                   map[string]string // metric name -> ID
	plan, fx, byPeriod       string            // grid IDs
	dims                     map[string][]string
}

func (f *restrictedFixture) req(method, path, persona string, body any) (int, []byte) {
	f.t.Helper()
	var buf []byte
	if body != nil {
		var err error
		if buf, err = json.Marshal(body); err != nil {
			f.t.Fatalf("encode body: %v", err)
		}
	}
	r, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	r.Header.Set("X-Dev-User", persona)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-App-Id", f.appID)
	r.Header.Set("X-Revision-Id", f.revID)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		f.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	out := new(bytes.Buffer)
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}

func (f *restrictedFixture) call(method, path, persona string, body any) string {
	f.t.Helper()
	status, raw := f.req(method, path, persona, body)
	if status < 200 || status >= 300 {
		f.t.Fatalf("%s %s as %s: status %d\n%s", method, path, persona, status, raw)
	}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	id, _ := parsed["id"].(string)
	return id
}

func (f *restrictedFixture) dev_(method, path string, body any) string {
	f.t.Helper()
	return f.call(method, path, f.dev, body)
}

var months = []string{"2026-01", "2026-02", "2026-03", "2026-04", "2026-05", "2026-06"}

// revenue at (region, month m = 1..6): UK 100+m, DE 200+m, US 300+m.
func revenueAt(region string, m int) float64 {
	return map[string]float64{"UK": 100, "DE": 200, "US": 300}[region] + float64(m)
}

func setupRestrictedFixture(t *testing.T) *restrictedFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	f := &restrictedFixture{t: t, pool: pool, dev: "rr-dev", admin: "rr-admin", viewer: "rr-viewer",
		members: map[string]string{}, metric: map[string]string{}, dims: map[string][]string{}}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	// Tenant scaffolding and the three people; the model itself is built
	// over HTTP below.
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('RR Co', 'enterprise') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'W') RETURNING id::text`, cust)
	f.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'A', 'planning') RETURNING id::text`, ws, cust)
	f.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, f.appID)
	f.revID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelID)
	if _, err := pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, f.revID, f.modelID); err != nil {
		t.Fatal(err)
	}
	mk := func(sub, role string) string {
		uid := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1 || '@x.co', $1, $2::uuid) RETURNING id::text`, sub, cust)
		if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, $3::uuid)`, uid, role, ws); err != nil {
			t.Fatal(err)
		}
		return uid
	}
	mk(f.dev, "developer")
	mk(f.admin, "business_admin")
	f.viewerID = mk(f.viewer, "business_user")
	t.Setenv("DEV_MODE", "true")
	f.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(f.srv.Close)
	f.build()
	return f
}

func (f *restrictedFixture) build() {
	dims := "/api/developer/dimensions"
	f.region = f.dev_("POST", dims, map[string]any{"name": "region", "revision_id": f.revID, "dimension_type": "standard"})
	f.currency = f.dev_("POST", dims, map[string]any{"name": "currency", "revision_id": f.revID, "dimension_type": "standard"})
	f.period = f.dev_("POST", dims, map[string]any{"name": "period", "revision_id": f.revID, "dimension_type": "time",
		"time_granularity": "month", "fiscal_year_start_month": 1})
	add := func(dim, code, parent string, extra map[string]any) {
		body := map[string]any{"code": code, "label": code}
		if parent != "" {
			body["parent_member_id"] = f.members[parent]
		}
		for k, v := range extra {
			body[k] = v
		}
		f.members[code] = f.dev_("POST", dims+"/"+dim+"/members", body)
	}
	add(f.region, "EMEA", "", nil)
	add(f.region, "UK", "EMEA", nil)
	add(f.region, "DE", "EMEA", nil)
	add(f.region, "US", "", nil)
	for _, c := range []string{"GBP", "EUR", "USD"} {
		add(f.currency, c, "", nil)
	}
	add(f.period, "H1", "", nil)
	add(f.period, "Q1", "H1", nil)
	add(f.period, "Q2", "H1", nil)
	ends := []string{"31", "28", "31", "30", "31", "30"}
	for i, code := range months {
		parent := "Q1"
		if i >= 3 {
			parent = "Q2"
		}
		add(f.period, code, parent, map[string]any{"period_start": code + "-01", "period_end": code + "-" + ends[i]})
	}
	props := dims + "/" + f.region + "/properties"
	f.dev_("POST", props, map[string]any{"name": "segment", "data_type": "text"})
	f.dev_("POST", props, map[string]any{"name": "currency", "data_type": "text"})
	f.dev_("POST", props, map[string]any{"name": "factor", "data_type": "number"})
	for code, p := range map[string]map[string]string{
		"UK": {"segment": "SMB", "currency": "GBP", "factor": "2"},
		"DE": {"segment": "SMB", "currency": "EUR", "factor": "3"},
		"US": {"segment": "ENT", "currency": "USD", "factor": "4"},
	} {
		body := map[string]any{"code": code, "label": code, "properties": p}
		if code != "US" {
			body["parent_member_id"] = f.members["EMEA"]
		}
		f.dev_("PATCH", dims+"/"+f.region+"/members/"+f.members[code], body)
	}

	grid := func(name string, dimIDs ...string) string {
		id := f.dev_("POST", "/api/developer/grids", map[string]any{"name": name, "revision_id": f.revID})
		for _, d := range dimIDs {
			f.dev_("POST", "/api/developer/grids/"+id+"/dimensions/"+d, nil)
		}
		return id
	}
	f.plan = grid("Plan", f.region, f.period)
	f.fx = grid("FX", f.currency)
	f.byPeriod = grid("ByPeriod", f.period)
	metric := func(gridID, name, text string) {
		body := map[string]any{"name": name, "is_input": text == "", "formula": text, "revision_id": f.revID,
			"agg_rule": "sum", "format": "number", "time_summary": "sum"}
		f.metric[name] = f.dev_("POST", "/api/developer/metrics", body)
		f.dev_("POST", "/api/developer/grids/"+gridID+"/metrics/"+f.metric[name], nil)
	}
	metric(f.plan, "revenue", "")
	metric(f.fx, "fx_rate", "")
	for _, m := range []struct{ name, text string }{
		{"us_rev", `LOOKUP(revenue, region, "US")`},
		{"smb_rev", `SUMIFS(revenue, region.segment, "SMB")`},
		{"conv", `revenue * LOOKUP(fx_rate, currency, region.currency)`},
		{"prev", `PREVIOUS(revenue)`},
		{"dyn", `LAG(revenue, IF(region = "UK", 1, 2), 0)`},
		{"yv", `YEARVALUE(revenue)`},
		{"ts_q1", `TIMESUM(revenue, "Q1", "Q1")`},
		{"ts_q2", `TIMESUM(revenue, "Q2", "Q2")`},
		{"dep", `us_rev * 2`},
		{"scaled", `revenue * region.factor`},
	} {
		metric(f.plan, m.name, m.text)
	}
	metric(f.byPeriod, "prev_all", `PREVIOUS(revenue)`)

	for _, rg := range []string{"UK", "DE", "US"} {
		for i, p := range months {
			f.dev_("POST", "/api/cells", map[string]any{"model_id": f.modelID, "metric_id": f.metric["revenue"], "revision_id": f.revID,
				"dim_codes": map[string]string{f.region: rg, f.period: p}, "value": revenueAt(rg, i+1)})
		}
	}
	for c, v := range map[string]float64{"GBP": 2, "EUR": 3, "USD": 1} {
		f.dev_("POST", "/api/cells", map[string]any{"model_id": f.modelID, "metric_id": f.metric["fx_rate"], "revision_id": f.revID,
			"dim_codes": map[string]string{f.currency: c}, "value": v})
	}

	f.loadMetricDims()

	// Recalculation runs asynchronously after each write: wait for the
	// last values of every metric.
	f.await("us_rev", map[string]string{f.region: "UK", f.period: "2026-06"}, 306)
	f.await("smb_rev", map[string]string{f.region: "US", f.period: "2026-06"}, 312)
	f.await("conv", map[string]string{f.region: "DE", f.period: "2026-06"}, 618)
	f.await("prev", map[string]string{f.region: "US", f.period: "2026-06"}, 305)
	f.await("dyn", map[string]string{f.region: "US", f.period: "2026-06"}, 304)
	f.await("yv", map[string]string{f.region: "US", f.period: "2026-06"}, 1821)
	f.await("ts_q1", map[string]string{f.region: "US", f.period: "2026-06"}, 906)
	f.await("ts_q2", map[string]string{f.region: "US", f.period: "2026-06"}, 915)
	f.await("dep", map[string]string{f.region: "US", f.period: "2026-06"}, 612)
	f.await("scaled", map[string]string{f.region: "US", f.period: "2026-06"}, 1224)
	f.await("prev_all", map[string]string{f.period: "2026-06"}, 615)
}

// loadMetricDims records every metric's own dimension order, as the grid
// keys cells.
func (f *restrictedFixture) loadMetricDims() {
	f.t.Helper()
	var g struct {
		AllMetrics []metricRow `json:"all_metrics"`
	}
	_, raw := f.req("GET", "/api/grid?meta_only=1", f.dev, nil)
	if err := json.Unmarshal(raw, &g); err != nil {
		f.t.Fatalf("meta grid: %v\n%s", err, raw)
	}
	for _, m := range g.AllMetrics {
		f.dims[m.ID] = m.DimensionIDs
	}
}

// await polls calc_result for metric at combo (dimension ID -> code).
func (f *restrictedFixture) await(metric string, combo map[string]string, want float64) {
	f.t.Helper()
	raw, _ := json.Marshal(combo)
	deadline := time.Now().Add(30 * time.Second)
	var last float64
	seen := false
	for time.Now().Before(deadline) {
		var v float64
		err := f.pool.QueryRow(context.Background(), `
			SELECT value FROM runtime.calc_result
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND dim_members=$4::jsonb
			ORDER BY calc_at DESC LIMIT 1`, f.modelID, f.revID, f.metric[metric], string(raw)).Scan(&v)
		if err == nil {
			last, seen = v, true
			if nearly(v, want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !seen {
		f.t.Fatalf("%s at %v: no calc_result row (want %v)", metric, combo, want)
	}
	f.t.Fatalf("%s at %v = %v, want %v (waited 30s)", metric, combo, last, want)
}

// hide replaces the viewer's rules with "hidden" on the given members.
func (f *restrictedFixture) hide(codes ...string) {
	f.t.Helper()
	rules := []map[string]string{}
	for _, c := range codes {
		rules = append(rules, map[string]string{"rule_type": "dimension_member", "ref_id": f.members[c], "access": "hidden"})
	}
	f.call("PUT", "/api/business-admin/users/"+f.viewerID+"/access-rules", f.admin, map[string]any{"rules": rules})
}

type gridRead struct {
	Cells    map[string]float64 `json:"cells"`
	Totals   map[string]float64 `json:"totals"`
	Withheld []string           `json:"withheld"`
}

func (f *restrictedFixture) grid(persona, gridID, scope string) gridRead {
	f.t.Helper()
	path := "/api/grid?grid_def_id=" + gridID
	if scope != "" {
		path += "&scope=" + url.QueryEscape(scope)
	}
	status, raw := f.req("GET", path, persona, nil)
	if status != http.StatusOK {
		f.t.Fatalf("grid %s as %s: %d %s", path, persona, status, raw)
	}
	var g gridRead
	if err := json.Unmarshal(raw, &g); err != nil {
		f.t.Fatalf("grid: %v", err)
	}
	return g
}

// key is the grid cell key of metric at (region, period) codes; "" skips a
// dimension the metric does not have.
func (f *restrictedFixture) key(metric string, codes map[string]string) string {
	id := f.metric[metric]
	parts := []string{id}
	for _, d := range f.dims[id] {
		parts = append(parts, codes[d])
	}
	return strings.Join(parts, ":")
}

func (f *restrictedFixture) at(metric, region, period string) string {
	return f.key(metric, map[string]string{f.region: region, f.period: period})
}

func (g gridRead) isWithheld(k string) bool {
	for _, w := range g.Withheld {
		if w == k {
			return true
		}
	}
	return false
}

// served asserts the cell is present with want and not withheld.
func (f *restrictedFixture) served(g gridRead, label, k string, want float64) {
	f.t.Helper()
	v, ok := g.Cells[k]
	if !ok || !nearly(v, want) || g.isWithheld(k) {
		f.t.Errorf("%s: %s = %v (present=%v, withheld=%v), want %v served", label, k, v, ok, g.isWithheld(k), want)
	}
}

// withheld asserts the cell is absent and listed as withheld.
func (f *restrictedFixture) withheld(g gridRead, label, k string) {
	f.t.Helper()
	if v, ok := g.Cells[k]; ok || !g.isWithheld(k) {
		f.t.Errorf("%s: %s present=%v (value %v), withheld=%v; want withheld", label, k, ok, v, g.isWithheld(k))
	}
}

func (f *restrictedFixture) totalWithheld(g gridRead, label, metric string) {
	f.t.Helper()
	id := f.metric[metric]
	if v, ok := g.Totals[id]; ok || !g.isWithheld(id) {
		f.t.Errorf("%s: total of %s present=%v (value %v), withheld=%v; want withheld", label, metric, ok, v, g.isWithheld(id))
	}
}

func TestRestrictedViewerWithheldCells(t *testing.T) {
	f := setupRestrictedFixture(t)

	// ── US hidden: a LOOKUP of US, and everything built on it, is withheld;
	// SUMIFS over SMB (US is ENT) and the member-local FX LOOKUP are served
	// with their exact values; a metric on period alone aggregated US.
	f.hide("US")
	g := f.grid(f.viewer, f.plan, "")
	for _, p := range []string{"2026-02", "2026-05"} {
		f.withheld(g, "LOOKUP of the hidden US", f.at("us_rev", "UK", p))
		f.withheld(g, "an ordinary metric reading a withheld cell", f.at("dep", "UK", p))
	}
	f.withheld(g, "the LOOKUP's aggregate period", f.at("us_rev", "UK", "Q1"))
	f.totalWithheld(g, "LOOKUP of the hidden US", "us_rev")
	f.totalWithheld(g, "an ordinary metric reading a withheld cell", "dep")
	f.served(g, "SUMIFS where only a non-matching member is hidden", f.at("smb_rev", "UK", "2026-02"), 304)
	f.served(g, "SUMIFS aggregate period", f.at("smb_rev", "DE", "Q1"), 302+304+306)
	if v, ok := g.Totals[f.metric["smb_rev"]]; !ok || !nearly(v, 2*(1800+42)) {
		t.Errorf("SUMIFS total over the viewer's regions = %v (present=%v), want %v", v, ok, 2*(1800+42))
	}
	f.served(g, "FX LOOKUP by region.currency", f.at("conv", "UK", "2026-02"), 204)
	f.served(g, "FX LOOKUP by region.currency", f.at("conv", "DE", "2026-02"), 606)
	f.served(g, "dim.property metric recomputed under a restriction", f.at("scaled", "DE", "2026-03"), 609)
	f.served(g, "a window with nothing hidden", f.at("prev", "UK", "2026-03"), 102)
	if _, ok := g.Cells[f.at("us_rev", "US", "2026-02")]; ok {
		t.Error("the hidden member's own cells must not appear at all")
	}
	gp := f.grid(f.viewer, f.byPeriod, "")
	for _, p := range months[1:] {
		f.withheld(gp, "coarse grain: period-only metric over every region", f.key("prev_all", map[string]string{f.period: p}))
	}
	f.totalWithheld(gp, "coarse grain", "prev_all")

	// ── DE hidden (an SMB region): SUMIFS by segment now matches it.
	f.hide("DE")
	g = f.grid(f.viewer, f.plan, "")
	f.withheld(g, "SUMIFS by property where a hidden member matches", f.at("smb_rev", "UK", "2026-02"))
	f.totalWithheld(g, "SUMIFS by property where a hidden member matches", "smb_rev")
	f.served(g, "LOOKUP of a visible member", f.at("us_rev", "UK", "2026-02"), 302)
	f.served(g, "ordinary metric over a served cell", f.at("dep", "UK", "2026-02"), 604)
	f.served(g, "FX LOOKUP, other region", f.at("conv", "UK", "2026-02"), 204)
	f.served(g, "FX LOOKUP, other region", f.at("conv", "US", "2026-02"), 302)

	// ── EUR hidden (a currency): exactly the region reading it is withheld.
	f.hide("EUR")
	g = f.grid(f.viewer, f.plan, "")
	f.withheld(g, "FX LOOKUP of the hidden currency", f.at("conv", "DE", "2026-02"))
	f.served(g, "FX LOOKUP of a visible currency", f.at("conv", "UK", "2026-02"), 204)

	// ── January hidden: windows that reach it are withheld; the aggregate
	// periods and totals holding a withheld cell are too; ranges that do
	// not reach it are served.
	f.hide("2026-01")
	g = f.grid(f.viewer, f.plan, "")
	f.withheld(g, "PREVIOUS at February", f.at("prev", "UK", "2026-02"))
	f.served(g, "PREVIOUS at March", f.at("prev", "UK", "2026-03"), 102)
	f.withheld(g, "Q1 holds the withheld February", f.at("prev", "UK", "Q1"))
	f.withheld(g, "H1 holds the withheld February", f.at("prev", "UK", "H1"))
	f.served(g, "Q2 of PREVIOUS", f.at("prev", "UK", "Q2"), 103+104+105)
	f.totalWithheld(g, "PREVIOUS", "prev")
	f.withheld(g, "dynamic LAG offset", f.at("dyn", "UK", "2026-06"))
	f.withheld(g, "YEARVALUE", f.at("yv", "UK", "2026-06"))
	f.withheld(g, "TIMESUM over Q1", f.at("ts_q1", "UK", "2026-06"))
	f.served(g, "TIMESUM over Q2", f.at("ts_q2", "UK", "2026-03"), 315)
	f.served(g, "LOOKUP of another region, same period", f.at("us_rev", "UK", "2026-03"), 303)
	gp = f.grid(f.viewer, f.byPeriod, "")
	f.withheld(gp, "single-dimension PREVIOUS at February", f.key("prev_all", map[string]string{f.period: "2026-02"}))
	f.served(gp, "single-dimension PREVIOUS at March", f.key("prev_all", map[string]string{f.period: "2026-03"}), 606)
	f.withheld(gp, "single-dimension Q1", f.key("prev_all", map[string]string{f.period: "Q1"}))
	f.served(gp, "single-dimension Q2 (leaves only, never the persisted Q2 row again)",
		f.key("prev_all", map[string]string{f.period: "Q2"}), 609+612+615)

	// ── The metric list: the whole-model calculated totals include every
	// member, so a caller with any hidden member gets null.
	status, raw := f.req("GET", "/api/metrics", f.viewer, nil)
	if status != http.StatusOK {
		t.Fatalf("/api/metrics as viewer: %d %s", status, raw)
	}
	var list []metricRow
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if !m.IsInput && m.Value != nil {
			t.Errorf("/api/metrics for a restricted caller: %s value = %v, want null", m.Name, *m.Value)
		}
	}
	_, raw = f.req("GET", "/api/metrics", f.dev, nil)
	list = nil
	_ = json.Unmarshal(raw, &list)
	for _, m := range list {
		if m.Name == "smb_rev" && (m.Value == nil || !nearly(*m.Value, 3*(1800+42))) {
			t.Errorf("/api/metrics for an unrestricted caller: smb_rev = %v, want %v", m.Value, 3*(1800+42))
		}
	}
}

// TestScopePinServesPersistedCells: a caller with no hidden member and only
// a scope pin sees every served cell exactly as persisted — the same cells
// the unscoped read returns — and nothing withheld.
func TestScopePinServesPersistedCells(t *testing.T) {
	f := setupRestrictedFixture(t)
	f.hide() // no rules
	served := []string{"us_rev", "smb_rev", "conv", "prev", "dyn", "yv", "ts_q1", "ts_q2", "prev_all"}
	for _, tc := range []struct {
		label string
		dim   string
		code  string
		in    map[string]bool
	}{
		{"region pin", f.region, "EMEA", map[string]bool{"EMEA": true, "UK": true, "DE": true}},
		{"period pin", f.period, "Q1", map[string]bool{"Q1": true, "2026-01": true, "2026-02": true, "2026-03": true}},
	} {
		scope := fmt.Sprintf(`{%q:%q}`, tc.dim, tc.code)
		for _, gridID := range []string{f.plan, f.byPeriod} {
			full := f.grid(f.viewer, gridID, "")
			pinned := f.grid(f.viewer, gridID, scope)
			if len(full.Withheld) != 0 || len(pinned.Withheld) != 0 {
				t.Errorf("%s: nothing is hidden, yet withheld %v / %v", tc.label, full.Withheld, pinned.Withheld)
			}
			for _, name := range served {
				id := f.metric[name]
				pos := -1
				for i, d := range f.dims[id] {
					if d == tc.dim {
						pos = i
					}
				}
				checked := 0
				for k, want := range full.Cells {
					parts := strings.Split(k, ":")
					if parts[0] != id || (pos >= 0 && !tc.in[parts[1+pos]]) {
						continue
					}
					checked++
					if got, ok := pinned.Cells[k]; !ok || got != want {
						t.Errorf("%s: %s %s = %v (present=%v), persisted %v", tc.label, name, k, got, ok, want)
					}
				}
				for k, got := range pinned.Cells {
					if !strings.HasPrefix(k, id+":") {
						continue
					}
					if want, ok := full.Cells[k]; ok && got != want {
						t.Errorf("%s: %s %s = %v, persisted %v", tc.label, name, k, got, want)
					}
				}
				if checked == 0 && gridID == f.plan && name != "prev_all" {
					t.Errorf("%s: no %s cells compared", tc.label, name)
				}
			}
		}
	}
	// An aggregate period computed from the persisted leaves, never with
	// the persisted aggregate rows summed back in.
	pinned := f.grid(f.viewer, f.byPeriod, fmt.Sprintf(`{%q:"Q1"}`, f.period))
	if got := pinned.Cells[f.key("prev_all", map[string]string{f.period: "Q1"})]; !nearly(got, 0+603+606) {
		t.Errorf("prev_all Q1 under a pin = %v, want %v", got, 0+603+606)
	}
	pinned = f.grid(f.viewer, f.plan, fmt.Sprintf(`{%q:"EMEA"}`, f.region))
	if got := pinned.Cells[f.at("us_rev", "UK", "Q1")]; !nearly(got, 301+302+303) {
		t.Errorf("us_rev UK Q1 under a pin = %v, want %v", got, 301+302+303)
	}
	// dim.property metric recomputed under a scope, exactly.
	f.served(pinned, "dim.property metric under a scope", f.at("scaled", "DE", "2026-04"), 612)
	// The pinned total agrees with the scheduler's precomputed slice row.
	slice, _ := json.Marshal(map[string]string{f.region: "EMEA"})
	for _, name := range []string{"us_rev", "smb_rev", "prev", "conv"} {
		var want float64
		if err := f.pool.QueryRow(context.Background(), `
			SELECT value FROM runtime.calc_result WHERE metric_id=$1::uuid AND dim_members=$2::jsonb
			ORDER BY calc_at DESC LIMIT 1`, f.metric[name], string(slice)).Scan(&want); err != nil {
			t.Fatalf("slice row of %s: %v", name, err)
		}
		if got, ok := pinned.Totals[f.metric[name]]; !ok || !nearly(got, want) {
			t.Errorf("pinned total of %s = %v (present=%v), slice row %v", name, got, ok, want)
		}
	}
}

// TestRestrictedViewerChartData applies the same rule through chart-data: a
// withheld point is null, every other point is the persisted value.
func TestRestrictedViewerChartData(t *testing.T) {
	f := setupRestrictedFixture(t)
	dash := f.dev_("POST", "/api/developer/dashboards", map[string]any{"name": "D", "revision_id": f.revID})
	widget := func(plotted string, metrics []string, ctx map[string]string) string {
		ids := make([]string, len(metrics))
		for i, m := range metrics {
			ids[i] = f.metric[m]
		}
		return f.dev_("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{
			"widget_type": "chart", "ref_id": f.plan, "size_w": 400, "size_h": 300,
			"widget_props": map[string]any{"chart": map[string]any{
				"chart_type": "bar", "dimension_id": plotted, "metric_ids": ids, "context_defaults": ctx}},
		})
	}
	byRegion := widget(f.region, []string{"us_rev", "smb_rev", "conv", "dep", "scaled"}, map[string]string{f.period: "2026-02"})
	byPeriod := widget(f.period, []string{"prev", "ts_q2", "yv"}, map[string]string{f.region: "UK"})

	type series struct {
		MetricID string     `json:"metric_id"`
		Values   []*float64 `json:"values"`
	}
	chart := func(widgetID string) map[string]map[string]*float64 {
		t.Helper()
		status, raw := f.req("POST", "/api/dashboard-widgets/"+widgetID+"/chart-data", f.viewer, map[string]any{"context": map[string]string{}})
		if status != http.StatusOK {
			t.Fatalf("chart-data: %d %s", status, raw)
		}
		var out struct {
			Categories []struct {
				Key string `json:"key"`
			} `json:"categories"`
			Series []series `json:"series"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		res := map[string]map[string]*float64{}
		for _, s := range out.Series {
			res[s.MetricID] = map[string]*float64{}
			for i, c := range out.Categories {
				res[s.MetricID][c.Key] = s.Values[i]
			}
		}
		return res
	}
	point := func(label string, c map[string]map[string]*float64, metric, category string, want *float64) {
		t.Helper()
		got, plotted := c[f.metric[metric]][category]
		switch {
		case !plotted:
			t.Errorf("%s: %s at %s not plotted", label, metric, category)
		case want == nil && got != nil:
			t.Errorf("%s: %s at %s = %v, want null (withheld)", label, metric, category, *got)
		case want != nil && (got == nil || !nearly(*got, *want)):
			t.Errorf("%s: %s at %s = %v, want %v", label, metric, category, got, *want)
		}
	}
	v := func(x float64) *float64 { return &x }

	f.hide("US")
	c := chart(byRegion)
	point("US hidden", c, "us_rev", "UK", nil)
	point("US hidden", c, "dep", "UK", nil)
	point("US hidden", c, "smb_rev", "UK", v(304))
	point("US hidden", c, "smb_rev", "EMEA", v(608))
	point("US hidden", c, "conv", "DE", v(606))
	point("US hidden", c, "scaled", "UK", v(204))

	f.hide("DE")
	c = chart(byRegion)
	point("DE hidden", c, "smb_rev", "UK", nil)
	point("DE hidden", c, "us_rev", "UK", v(302))
	point("DE hidden", c, "dep", "UK", v(604))
	point("DE hidden", c, "conv", "UK", v(204))

	f.hide("2026-01")
	c = chart(byPeriod)
	point("January hidden", c, "prev", "2026-02", nil)
	point("January hidden", c, "prev", "2026-03", v(102))
	point("January hidden", c, "prev", "Q1", nil)
	point("January hidden", c, "prev", "Q2", v(103+104+105))
	point("January hidden", c, "ts_q2", "2026-03", v(315))
	point("January hidden", c, "yv", "2026-06", nil)

	// Unrestricted: the persisted values.
	f.hide()
	c = chart(byPeriod)
	point("unrestricted", c, "prev", "2026-02", v(101))
	point("unrestricted", c, "yv", "2026-06", v(621))
}

// TestGridFailsClosedWhenAccessRulesCannotBeRead: a caller whose rules
// cannot be read is refused, never served as if unrestricted.
func TestGridFailsClosedWhenAccessRulesCannotBeRead(t *testing.T) {
	f := setupRestrictedFixture(t)
	f.hide("US")
	if _, err := f.pool.Exec(context.Background(), `ALTER TABLE identity.user_access_rule RENAME TO user_access_rule_gone`); err != nil {
		t.Fatal(err)
	}
	status, raw := f.req("GET", "/api/grid?grid_def_id="+f.plan, f.viewer, nil)
	if status != http.StatusInternalServerError {
		t.Errorf("grid with unreadable access rules: status %d, want 500\n%.300s", status, raw)
	}
	if !bytes.Contains(raw, []byte("load access rules")) || bytes.Contains(raw, []byte(`"cells"`)) {
		t.Errorf("grid with unreadable access rules must fail on that query and serve no data: %.300s", raw)
	}
}

// addMetric creates a calculated metric on the plan grid with aggRule.
func (f *restrictedFixture) addMetric(name, text, aggRule string) {
	f.t.Helper()
	f.metric[name] = f.dev_("POST", "/api/developer/metrics", map[string]any{"name": name, "formula": text,
		"revision_id": f.revID, "agg_rule": aggRule, "format": "number", "time_summary": "sum"})
	f.dev_("POST", "/api/developer/grids/"+f.plan+"/metrics/"+f.metric[name], nil)
	f.loadMetricDims()
}

// recalc rewrites one revenue cell with its own value: the write triggers
// the recalculation of every metric reading revenue, as for a user's edit.
func (f *restrictedFixture) recalc() {
	f.t.Helper()
	f.dev_("POST", "/api/cells", map[string]any{"model_id": f.modelID, "metric_id": f.metric["revenue"], "revision_id": f.revID,
		"dim_codes": map[string]string{f.region: "UK", f.period: "2026-01"}, "value": revenueAt("UK", 1)})
}

// chartWidget adds a bar chart over the plan grid to dashboard dash.
func (f *restrictedFixture) chartWidget(dash, plotted string, metrics []string, ctx map[string]string) string {
	f.t.Helper()
	ids := make([]string, len(metrics))
	for i, m := range metrics {
		ids[i] = f.metric[m]
	}
	return f.dev_("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{
		"widget_type": "chart", "ref_id": f.plan, "size_w": 400, "size_h": 300,
		"widget_props": map[string]any{"chart": map[string]any{
			"chart_type": "bar", "dimension_id": plotted, "metric_ids": ids, "context_defaults": ctx}},
	})
}

// chartPoints reads a widget's chart-data as persona: metric name ->
// category key -> value (nil = no point).
func (f *restrictedFixture) chartPoints(persona, widgetID string) map[string]map[string]*float64 {
	f.t.Helper()
	status, raw := f.req("POST", "/api/dashboard-widgets/"+widgetID+"/chart-data", persona, map[string]any{"context": map[string]string{}})
	if status != http.StatusOK {
		f.t.Fatalf("chart-data: %d %s", status, raw)
	}
	var out struct {
		Categories []struct {
			Key string `json:"key"`
		} `json:"categories"`
		Series []struct {
			MetricID string     `json:"metric_id"`
			Values   []*float64 `json:"values"`
		} `json:"series"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatal(err)
	}
	byID := map[string]string{}
	for name, id := range f.metric {
		byID[id] = name
	}
	res := map[string]map[string]*float64{}
	for _, s := range out.Series {
		name := byID[s.MetricID]
		res[name] = map[string]*float64{}
		for i, c := range out.Categories {
			res[name][c.Key] = s.Values[i]
		}
	}
	return res
}

// point asserts one chart point: want nil = null (withheld or missing).
func (f *restrictedFixture) point(label string, c map[string]map[string]*float64, metric, category string, want *float64) {
	f.t.Helper()
	got, plotted := c[metric][category]
	switch {
	case !plotted:
		f.t.Errorf("%s: %s at %s not plotted", label, metric, category)
	case want == nil && got != nil:
		f.t.Errorf("%s: %s at %s = %v, want null (withheld)", label, metric, category, *got)
	case want != nil && got == nil:
		f.t.Errorf("%s: %s at %s = null, want %v", label, metric, category, *want)
	case want != nil && !nearly(*got, *want):
		f.t.Errorf("%s: %s at %s = %v, want %v", label, metric, category, *got, *want)
	}
}

// TestRestrictedViewerWindowedMemberLocal: a member-local LOOKUP member
// inside a time window is evaluated by the scheduler at the SHIFTED period
// (the dimension names and member context are rebound to it), so the read
// set must follow it there — on the grid and in chart-data alike.
func TestRestrictedViewerWindowedMemberLocal(t *testing.T) {
	f := setupRestrictedFixture(t)
	f.addMetric("pl1", `PREVIOUS(LOOKUP(revenue, period, period))`, "sum")
	f.addMetric("pl2", `PREVIOUS(LOOKUP(revenue, period, PARENT(period)))`, "sum")
	f.addMetric("pl3", `PREVIOUS(LOOKUP(revenue, region, IF(period = "2026-01", "US", "UK")))`, "sum")
	f.recalc()
	f.await("pl1", map[string]string{f.region: "UK", f.period: "2026-06"}, 105)
	f.await("pl2", map[string]string{f.region: "UK", f.period: "2026-06"}, 104+105+106)
	f.await("pl3", map[string]string{f.region: "DE", f.period: "2026-06"}, 105)
	dash := f.dev_("POST", "/api/developer/dashboards", map[string]any{"name": "W", "revision_id": f.revID})
	ukByPeriod := f.chartWidget(dash, f.period, []string{"pl1", "pl2"}, map[string]string{f.region: "UK"})
	deByPeriod := f.chartWidget(dash, f.period, []string{"pl3"}, map[string]string{f.region: "DE"})
	v := func(x float64) *float64 { return &x }
	scope := fmt.Sprintf(`{%q:"UK"}`, f.region)

	f.hide("2026-01")
	for _, g := range []gridRead{f.grid(f.viewer, f.plan, ""), f.grid(f.viewer, f.plan, scope)} {
		f.withheld(g, "January hidden", f.at("pl1", "UK", "2026-02")) // UK's January
		f.served(g, "January hidden", f.at("pl1", "UK", "2026-03"), 102)
		f.withheld(g, "January hidden", f.at("pl2", "UK", "2026-04")) // Q1 at March
		f.served(g, "January hidden", f.at("pl2", "UK", "2026-05"), 104+105+106)
	}
	c := f.chartPoints(f.viewer, ukByPeriod)
	f.point("January hidden", c, "pl1", "2026-02", nil)
	f.point("January hidden", c, "pl1", "2026-03", v(102))
	f.point("January hidden", c, "pl2", "2026-04", nil)
	f.point("January hidden", c, "pl2", "2026-05", v(315))

	f.hide("2026-06")
	g := f.grid(f.viewer, f.plan, "")
	f.served(g, "June hidden", f.at("pl2", "UK", "2026-04"), 101+102+103) // Q1 at March
	f.withheld(g, "June hidden", f.at("pl2", "UK", "2026-05"))            // Q2 at April

	f.hide("US")
	g = f.grid(f.viewer, f.plan, "")
	f.withheld(g, "US hidden", f.at("pl3", "DE", "2026-02")) // January reads US
	f.served(g, "US hidden", f.at("pl3", "DE", "2026-03"), 102)
	c = f.chartPoints(f.viewer, deByPeriod)
	f.point("US hidden", c, "pl3", "2026-02", nil)
	f.point("US hidden", c, "pl3", "2026-03", v(102))
}

// TestRestrictedViewerRecurrenceReadsOnlyThePast: an opening/closing
// recurrence reads every EARLIER period, never a later one — a hidden
// future month withholds only its own cells.
func TestRestrictedViewerRecurrenceReadsOnlyThePast(t *testing.T) {
	f := setupRestrictedFixture(t)
	f.addMetric("closing", `revenue`, "sum")
	f.addMetric("opening", `PREVIOUS(closing)`, "sum")
	f.dev_("PATCH", "/api/developer/metrics/"+f.metric["closing"], map[string]any{"name": "closing", "formula": `opening + revenue`,
		"agg_rule": "sum", "format": "number", "time_summary": "sum"})
	f.recalc()
	f.await("closing", map[string]string{f.region: "UK", f.period: "2026-06"}, 101+102+103+104+105+106)

	f.hide("2026-06")
	g := f.grid(f.viewer, f.plan, "")
	f.served(g, "June hidden", f.at("closing", "UK", "2026-02"), 101+102)
	f.served(g, "June hidden", f.at("opening", "UK", "2026-03"), 101+102)
	f.served(g, "June hidden", f.at("closing", "UK", "2026-05"), 101+102+103+104+105)

	f.hide("2026-01")
	g = f.grid(f.viewer, f.plan, "")
	f.withheld(g, "January hidden", f.at("closing", "UK", "2026-05"))
	f.withheld(g, "January hidden", f.at("opening", "UK", "2026-03"))
}

// TestChartServesFormulaRuleRollupRow: a served metric with agg_rule
// formula is, at an aggregate point, the scheduler's persisted rollup row
// (the formula re-evaluated at the aggregate) — never the mean of its
// children — and that row is withheld when its subtree holds a hidden
// member.
func TestChartServesFormulaRuleRollupRow(t *testing.T) {
	f := setupRestrictedFixture(t)
	f.addMetric("us_share", `LOOKUP(revenue, region, "US") / revenue`, "formula")
	f.recalc()
	f.await("us_share", map[string]string{f.region: "EMEA", f.period: "2026-02"}, 302.0/304)
	dash := f.dev_("POST", "/api/developer/dashboards", map[string]any{"name": "S", "revision_id": f.revID})
	w := f.chartWidget(dash, f.region, []string{"us_share"}, map[string]string{f.period: "2026-02"})
	v := func(x float64) *float64 { return &x }

	c := f.chartPoints(f.viewer, w)
	f.point("unrestricted", c, "us_share", "EMEA", v(302.0/304))
	f.point("unrestricted", c, "us_share", "UK", v(302.0/102))
	g := f.grid(f.viewer, f.plan, "")
	f.served(g, "unrestricted grid", f.at("us_share", "EMEA", "2026-02"), 302.0/304)

	f.hide("DE")
	c = f.chartPoints(f.viewer, w)
	f.point("DE hidden", c, "us_share", "EMEA", nil)
	f.point("DE hidden", c, "us_share", "UK", v(302.0/102))
}

// grantBuilder gives the viewer the developer role in the application's
// workspace, keeping their access rules, and returns its revocation. A
// revision other than the active one is a builder's (revision_access.go),
// so the tests that read an old revision as a restricted person read it as
// a restricted builder — access rules bind builders like everyone else.
func (f *restrictedFixture) grantBuilder() (revoke func()) {
	f.t.Helper()
	ctx := context.Background()
	var raID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		SELECT $1::uuid, 'developer', workspace_id FROM core.application WHERE id = $2::uuid
		RETURNING id::text`, f.viewerID, f.appID).Scan(&raID); err != nil {
		f.t.Fatalf("grant builder: %v", err)
	}
	return func() {
		if _, err := f.pool.Exec(ctx, `DELETE FROM identity.role_assignment WHERE id::text = $1`, raID); err != nil {
			f.t.Fatalf("revoke builder: %v", err)
		}
	}
}

// assertOldRevisionClosed checks the viewer, a business user, is refused
// revisionID as one that does not exist.
func (f *restrictedFixture) assertOldRevisionClosed(label, revisionID string) {
	f.t.Helper()
	if status, raw := f.req("GET", "/api/grid?grid_def_id="+f.plan+"&revision_id="+revisionID, f.viewer, nil); status != http.StatusNotFound {
		f.errf(label, "business viewer reading a non-active revision: %d %.200s, want 404", status, raw)
	}
}
