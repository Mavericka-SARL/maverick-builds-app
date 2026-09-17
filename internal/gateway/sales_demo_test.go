// A small sales-planning model, built over the same HTTP API a developer uses,
// and then exercised the way the five people who touch it would.
//
// It exists to answer "does this actually work end to end" for the parts that
// are easy to break and hard to notice: multi-level rollups, the aggregation
// rules that are not sums, per-user access rules, workflow, spreadsheet import,
// and a form feeding a grid. Each of those is a subtest below.
//
// Three dimensions, each three levels deep, because two-level hierarchies hide
// a whole class of bug: a rollup that is correct at the root and wrong in the
// middle looks fine until someone opens a region.
//
//	Geography  World -> EMEA, AMER -> UK, DE / US, CA
//	Product    All   -> Hardware, Software -> Laptop, Monitor / License, Support
//	Period     FY26  -> H1, H2 -> Q1, Q2 / Q3, Q4
package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/salesdemo"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type salesDemo struct {
	t    *testing.T
	pool *pgxpool.Pool
	srv  *httptest.Server

	custID, wsID, appID, modelID, revID string

	geo, prod, period                  map[string]string // member code -> member id
	geoDim, prodDim, periodDim, gridID string
	metric                             map[string]string // metric name -> id
	model                              *salesdemo.Model

	// Personas, by keycloak_sub, and their identity.user ids.
	dev, admin                 string
	westRep, eastRep, ro       string
	westRepID, eastRepID, roID string
}

// ── HTTP, as a given persona ────────────────────────────────────────────────

func (d *salesDemo) req(method, path, persona string, body any) (int, []byte) {
	d.t.Helper()
	var buf []byte
	if body != nil {
		var err error
		if buf, err = json.Marshal(body); err != nil {
			d.t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method, d.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		d.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", persona)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-App-Id", d.appID)
	if d.revID != "" {
		req.Header.Set("X-Revision-Id", d.revID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	out := new(bytes.Buffer)
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}

// call fails the test on any non-2xx: setup steps must not fail quietly and
// leave a later assertion to report something misleading.
func (d *salesDemo) call(method, path, persona string, body any) map[string]any {
	d.t.Helper()
	status, raw := d.req(method, path, persona, body)
	if status < 200 || status >= 300 {
		d.t.Fatalf("%s %s as %s: status %d\n%s", method, path, persona, status, raw)
	}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return parsed
}

func idOf(m map[string]any) string {
	if v, ok := m["id"].(string); ok {
		return v
	}
	return ""
}

// ── Fixture ─────────────────────────────────────────────────────────────────

func setupSalesDemo(t *testing.T) *salesDemo {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")

	d := &salesDemo{
		t: t, pool: pool,
		geo: map[string]string{}, prod: map[string]string{}, period: map[string]string{},
		metric: map[string]string{},
		dev:    "demo-dev", admin: "demo-admin",
		westRep: "demo-west", eastRep: "demo-east", ro: "demo-readonly",
	}

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	// Tenant scaffolding straight into the database. It is not what this test
	// is about, and building it over HTTP would add a page of setup between
	// the reader and the model.
	d.custID = q(`INSERT INTO core.customer (name, plan) VALUES ('Demo Co', 'enterprise') RETURNING id::text`)
	d.wsID = q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Sales') RETURNING id::text`, d.custID)
	d.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Test app', 'planning') RETURNING id::text`, d.wsID, d.custID)
	d.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Sales Planning') RETURNING id::text`, d.appID)
	d.revID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, d.modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, d.revID, d.modelID)

	// Five people, each with only the role their part of the story needs.
	mk := func(sub, email, name, role string) string {
		uid := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,$2,$3,$4::uuid) RETURNING id::text`, sub, email, name, d.custID)
		exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, $3::uuid)`, uid, role, d.wsID)
		return uid
	}
	mk(d.dev, "dev@demo.co", "Dana Dev", "developer")
	mk(d.admin, "admin@demo.co", "Avery Admin", "business_admin")
	d.westRepID = mk(d.westRep, "west@demo.co", "Wes Trep", "business_user")
	d.eastRepID = mk(d.eastRep, "east@demo.co", "Eastin Rep", "business_user")
	d.roID = mk(d.ro, "readonly@demo.co", "Reed Only", "business_user")

	t.Setenv("DEV_MODE", "true")
	d.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(d.srv.Close)

	d.build()
	return d
}

// ── The model ───────────────────────────────────────────────────────────────

// build creates the model through internal/salesdemo, the same code
// cmd/seed-sales-planning runs against a real deployment. One definition, so a
// test cannot keep passing against a model that no longer matches the one
// anybody actually gets.
func (d *salesDemo) build() {
	d.t.Helper()
	caller := func(method, path string, body any) (map[string]any, error) {
		status, raw := d.req(method, path, d.dev, body)
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("%s %s: status %d: %s", method, path, status, raw)
		}
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		return parsed, nil
	}
	m, err := salesdemo.Build(caller, d.revID)
	if err != nil {
		d.t.Fatalf("build model: %v", err)
	}
	d.model = m
	d.geoDim, d.prodDim, d.periodDim, d.gridID = m.GeoDim, m.ProdDim, m.PeriodDim, m.GridID
	d.geo, d.prod, d.period, d.metric = m.Geo, m.Prod, m.Period, m.Metric
}

// ── Assertions ──────────────────────────────────────────────────────────────

func (d *salesDemo) calcValue(metric string, combo map[string]string) (float64, bool) {
	d.t.Helper()
	dimMembers := map[string]string{}
	for dimID, code := range combo {
		dimMembers[dimID] = code
	}
	raw, err := json.Marshal(dimMembers)
	if err != nil {
		d.t.Fatalf("encode combo: %v", err)
	}
	var v float64
	err = d.pool.QueryRow(context.Background(), `
		SELECT value FROM runtime.calc_result
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND dim_members=$4::jsonb
	`, d.modelID, d.revID, d.metric[metric], string(raw)).Scan(&v)
	if err != nil {
		return 0, false
	}
	return v, true
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 0.005 }

// awaitCalc waits for a metric to reach want. Import recalculates in a
// goroutine (see importUpload), so reading calc_result straight after the
// response is a race — and one that passes locally often enough to be
// believed. Polls for the value rather than sleeping a fixed amount, and
// reports what it actually saw when it gives up.
func (d *salesDemo) awaitCalc(metric string, combo map[string]string, want float64) {
	d.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last float64
	var seen bool
	for time.Now().Before(deadline) {
		v, ok := d.calcValue(metric, combo)
		if ok {
			last, seen = v, true
			if nearly(v, want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !seen {
		d.t.Errorf("%s never produced a calc_result row at %v (want %v)", metric, combo, want)
		return
	}
	d.t.Errorf("%s = %v, want %v (waited 15s)", metric, last, want)
}

// writeCell enters a value the way a planner does.
func (d *salesDemo) writeCell(persona, metric string, geo, prod, period string, value float64) (int, []byte) {
	d.t.Helper()
	return d.req("POST", "/api/cells", persona, map[string]any{
		"model_id":    d.modelID,
		"metric_id":   d.metric[metric],
		"revision_id": d.revID,
		// Keyed by dimension ID, not name: dim_codes is read against the
		// metric's own declared dimensions.
		"dim_codes": map[string]string{
			d.geoDim: geo, d.prodDim: prod, d.periodDim: period,
		},
		"value": value,
	})
}

// seedFacts writes the shared sample plan through the cell endpoint.
func (d *salesDemo) seedFacts() {
	d.t.Helper()
	caller := func(method, path string, body any) (map[string]any, error) {
		status, raw := d.req(method, path, d.dev, body)
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("%s %s: status %d: %s", method, path, status, raw)
		}
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		return parsed, nil
	}
	if err := d.model.WriteFacts(caller, d.modelID, d.revID, salesdemo.SampleFacts()); err != nil {
		d.t.Fatalf("seed facts: %v", err)
	}
}

// 1 — critical formulas and aggregation rules.
//
// The interesting assertions are the ones where the rules disagree with each
// other. If they all agreed, a metric silently falling back to "sum" would
// still look right.
func TestSalesDemoFormulasAndAggregationRules(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()

	leaf := func(geo, prod, period string) map[string]string {
		return map[string]string{d.geoDim: geo, d.prodDim: prod, d.periodDim: period}
	}
	check := func(name, metric string, combo map[string]string, want float64) {
		t.Helper()
		got, ok := d.calcValue(metric, combo)
		if !ok {
			t.Errorf("%s: no calc_result row for %s at %v", name, metric, combo)
			return
		}
		if !nearly(got, want) {
			t.Errorf("%s: %s = %v, want %v", name, metric, got, want)
		}
	}

	// Plain arithmetic, per intersection.
	check("UK laptop margin", "margin", leaf("UK", "LAPTOP", "Q1"), 200_000)   // 900k - 700k
	check("DE laptop margin", "margin", leaf("DE", "LAPTOP", "Q1"), 200_000)   // 500k - 300k
	check("US licence margin", "margin", leaf("US", "LICENSE", "Q1"), 300_000) // 400k - 100k

	// A percentage per intersection is well defined and differs between them,
	// which is what makes the total interesting.
	check("UK margin pct", "margin_pct", leaf("UK", "LAPTOP", "Q1"), 200.0/900.0*100)
	check("DE margin pct", "margin_pct", leaf("DE", "LAPTOP", "Q1"), 40)

	// The grand total. Revenue 1.9m, cost 1.14m, so margin 760k.
	total := map[string]string{}
	check("total margin", "margin", total, 760_000)

	// margin_pct's rule is 'formula': the total is 760k/1.9m, NOT the sum of
	// the four percentages (which would be about 175) nor their mean (~44).
	check("total margin pct", "margin_pct", total, 760.0/1900.0*100)

	// avg_price's rule is 'rate': total revenue over total units, 1.9m/1250 =
	// 1520. Averaging the four prices would give 1755 — the cheap high-volume
	// UK line is where the weight is, and a ratio summary is what respects it.
	check("blended price", "avg_price", total, 1_900_000.0/1250.0)

	// Attainment: 1.9m against a 1.85m target.
	check("total attainment", "attainment_pct", total, 1_900_000.0/1_850_000.0*100)
}

// TestSalesDemoScopedGridTotalsForCalcMetrics is the dashboard-KPI path: a grid
// read with a `scope=` pin (what a KPI following the shared dashboard context
// sends) must return, in `totals`, every CALC metric's total AT that slice —
// not 0. runtime.calc_result holds only leaf combos plus one '{}' whole-model
// aggregate; the '{}' row is filtered out by the scope predicate and there are
// no partial rollup rows, so a scoped calc metric has nothing to read directly.
// The fix routes scoped reads through the same per-combo re-resolution the
// hidden-member path uses, so the scoped total is the correct aggregate of the
// scoped slice — a plain sum for margin, and ratio-of-sums for margin_pct (NOT
// the meaningless sum/mean of per-combo percentages).
func TestSalesDemoScopedGridTotalsForCalcMetrics(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()

	revenue, margin, marginPct := d.metric["revenue"], d.metric["margin"], d.metric["margin_pct"]

	var whole struct {
		Cells      map[string]float64 `json:"cells"`
		Totals     map[string]float64 `json:"totals"`
		AllMetrics []struct {
			ID           string   `json:"id"`
			DimensionIDs []string `json:"dimension_ids"`
		} `json:"all_metrics"`
	}
	if _, raw := d.req("GET", "/api/grid?grid_id="+d.gridID, d.dev, nil); true {
		if err := json.Unmarshal(raw, &whole); err != nil {
			t.Fatalf("unscoped grid: %v\n%s", err, raw)
		}
	}

	// Where geography sits in each metric's composite cell key.
	geoIdx := -1
	for _, m := range whole.AllMetrics {
		if m.ID == margin {
			for i, did := range m.DimensionIDs {
				if did == d.geoDim {
					geoIdx = i
				}
			}
		}
	}
	if geoIdx < 0 {
		t.Fatalf("geography dim not found among margin's dimension_ids")
	}
	// Sum a metric's unscoped per-combo cells restricted to geo=UK — the
	// independently-derived expectation for the scoped total.
	sumUK := func(metricID string) float64 {
		var s float64
		for k, v := range whole.Cells {
			if !strings.HasPrefix(k, metricID+":") {
				continue
			}
			codes := strings.Split(k[len(metricID)+1:], ":")
			if geoIdx < len(codes) && codes[geoIdx] == "UK" {
				s += v
			}
		}
		return s
	}
	wantRevenue, wantMargin := sumUK(revenue), sumUK(margin)
	if wantRevenue == 0 || wantMargin == 0 {
		t.Fatalf("no UK cells found (revenue=%v margin=%v) — fixture changed", wantRevenue, wantMargin)
	}
	wantPct := wantMargin / wantRevenue * 100

	scope := fmt.Sprintf(`{%q:%q}`, d.geoDim, "UK")
	var scoped struct {
		Totals map[string]float64 `json:"totals"`
	}
	if _, raw := d.req("GET", "/api/grid?grid_id="+d.gridID+"&scope="+url.QueryEscape(scope), d.dev, nil); true {
		if err := json.Unmarshal(raw, &scoped); err != nil {
			t.Fatalf("scoped grid: %v\n%s", err, raw)
		}
	}

	if got := scoped.Totals[revenue]; !nearly(got, wantRevenue) {
		t.Errorf("scoped revenue total (geo=UK) = %v, want %v (sum of UK revenue cells)", got, wantRevenue)
	}
	if got := scoped.Totals[margin]; !nearly(got, wantMargin) {
		t.Errorf("scoped margin total (geo=UK) = %v, want %v (sum of UK margin cells); a calc metric had no scoped total before this fix", got, wantMargin)
	}
	if got := scoped.Totals[marginPct]; !nearly(got, wantPct) {
		t.Errorf("scoped margin_pct total (geo=UK) = %v, want %v (ratio-of-sums UK margin/UK revenue*100, NOT a sum of per-combo pcts)", got, wantPct)
	}
}

// TestSalesDemoSliceRowsMatchScopedRead is the correctness gate for the
// single-dimension slice precompute: every persisted slice row ({oneDim:
// member}, others aggregated) must equal what the live scoped grid read
// (scopeCalcCells) computes for that exact pin. If they ever diverge, the fast
// path (read the slice) and the fallback (recompute) would show different
// numbers for the same selector — so this pins them together across sum
// (margin), formula (margin_pct, attainment_pct), and rate (avg_price)
// metrics. avg_price found live: a rate metric with no slice row blocked the
// fast path for the WHOLE model, and the fallback's rate total was a mean of
// per-combo ratios rather than the ratio of scoped sums — both sides now use
// true Ratio semantics and must agree.
func TestSalesDemoSliceRowsMatchScopedRead(t *testing.T) {
	d := setupSalesDemo(t)
	// The forecast metrics include the dimension-conditional shapes
	// (quarter_weight, weighted_target) this test needs — the base Build
	// doesn't create them.
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
		t.Fatalf("build forecast: %v", err)
	}
	d.seedFacts()
	// Make sure the recompute (which writes the slice rows) has landed.
	d.awaitCalc("margin", map[string]string{}, 760_000)

	// quarter_weight/weighted_target are dimension-conditional (SWITCH on
	// period): they produce a value at EVERY combo, with or without input
	// data, so they specifically catch a scoped read whose combo universe
	// isn't trimmed to the pin (found live: a Q1 pin summing quarter_weight
	// over all four quarters' combos, 4x the slice row).
	metrics := []string{"margin", "margin_pct", "attainment_pct", "avg_price", "quarter_weight", "weighted_target"}
	pins := []struct{ dimID, code, label string }{
		{d.geoDim, "UK", "geo=UK"},
		{d.geoDim, "AMER", "geo=AMER(rollup)"},
		{d.periodDim, "Q1", "period=Q1"},
	}
	for _, p := range pins {
		scope := fmt.Sprintf(`{%q:%q}`, p.dimID, p.code)
		var g struct {
			Totals map[string]float64 `json:"totals"`
		}
		if _, raw := d.req("GET", "/api/grid?grid_id="+d.gridID+"&scope="+url.QueryEscape(scope), d.dev, nil); true {
			if err := json.Unmarshal(raw, &g); err != nil {
				t.Fatalf("scoped read %s: %v\n%s", p.label, err, raw)
			}
		}
		for _, m := range metrics {
			sliceVal, ok := d.calcValue(m, map[string]string{p.dimID: p.code})
			if !ok {
				t.Errorf("%s: no persisted slice row for %s", p.label, m)
				continue
			}
			total := g.Totals[d.metric[m]]
			if !nearly(sliceVal, total) {
				t.Errorf("%s: slice row for %s = %v, but scoped read total = %v — the fast path would disagree with the fallback", p.label, m, sliceVal, total)
			}
		}
	}
}

// TestSalesDemoRootPinsNormalizeAway proves the scope normalization that makes
// the fast paths reachable from a real dashboard: a selector left on "All"
// pins the dimension's universal root (WORLD / ALL_PROD / FY26), which
// constrains nothing and is dropped. So an all-roots scope reads identically
// to an unscoped read, and "one selector drilled, the rest on All" reduces to
// the single-dimension pin — the shape the slice fast path serves.
func TestSalesDemoRootPinsNormalizeAway(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	d.awaitCalc("margin", map[string]string{}, 760_000)

	totals := func(q string) map[string]float64 {
		var g struct {
			Totals map[string]float64 `json:"totals"`
		}
		if _, raw := d.req("GET", "/api/grid?grid_id="+d.gridID+q, d.dev, nil); true {
			if err := json.Unmarshal(raw, &g); err != nil {
				t.Fatalf("grid%s: %v\n%s", q, err, raw)
			}
		}
		return g.Totals
	}
	enc := func(m map[string]string) string {
		b, _ := json.Marshal(m)
		return "&scope=" + url.QueryEscape(string(b))
	}
	same := func(label string, a, b map[string]float64) {
		for id, av := range a {
			if !nearly(av, b[id]) {
				t.Errorf("%s: metric %s = %v vs %v", label, id, av, b[id])
			}
		}
	}

	unscoped := totals("")
	allRoots := totals(enc(map[string]string{d.geoDim: "WORLD", d.prodDim: "ALL_PROD", d.periodDim: "FY26"}))
	same("all-roots == unscoped", unscoped, allRoots)

	onlyQ1 := totals(enc(map[string]string{d.periodDim: "Q1"}))
	q1WithRoots := totals(enc(map[string]string{d.geoDim: "WORLD", d.periodDim: "Q1", d.prodDim: "ALL_PROD"}))
	same("Q1+roots == Q1 alone", onlyQ1, q1WithRoots)
}

// TestSalesDemoPartialSliceServing: a pin-dimensioned calc metric with NO
// slice row (the scheduler writes none where the formula can't evaluate — a
// sparse-data slice) must be OMITTED from a totals_only read's totals, not
// force the whole read onto the recompute, and not appear as a fabricated 0.
// Other metrics keep their slice-served values.
func TestSalesDemoPartialSliceServing(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	d.awaitCalc("margin", map[string]string{}, 760_000)

	// Simulate the sparse-slice shape: remove margin_pct's {geo:UK} slice row.
	if _, err := d.pool.Exec(context.Background(), `
		DELETE FROM runtime.calc_result
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
		  AND dim_members = jsonb_build_object($4::text, 'UK')
	`, d.modelID, d.revID, d.metric["margin_pct"], d.geoDim); err != nil {
		t.Fatalf("delete slice row: %v", err)
	}

	scope := fmt.Sprintf(`{%q:%q}`, d.geoDim, "UK")
	var g struct {
		Totals map[string]float64 `json:"totals"`
	}
	if _, raw := d.req("GET", "/api/grid?grid_id="+d.gridID+"&scope="+url.QueryEscape(scope)+"&totals_only=1", d.dev, nil); true {
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatalf("scoped totals_only: %v\n%s", err, raw)
		}
	}
	if _, present := g.Totals[d.metric["margin_pct"]]; present {
		t.Errorf("margin_pct present in totals (=%v) despite having no slice row — must be omitted, not fabricated", g.Totals[d.metric["margin_pct"]])
	}
	// The siblings are still served (slice values, not a full fallback with
	// margin_pct recomputed back in — its absence above proves which path ran).
	if v := g.Totals[d.metric["margin"]]; !nearly(v, 200_000) {
		t.Errorf("margin (geo=UK) = %v, want 200000 (UK: 900k revenue − 700k cost) — sibling metrics must still be served", v)
	}
}

// TestSalesDemoTiedTimestampRowsStayConsistent pins the fix for a live-found
// divergence: a bulk import writes many rows in ONE transaction, so several
// rows at the SAME cell share an identical entered_at (now() is
// transaction-fixed). Every latest-wins reader dedupes with DISTINCT ON ...
// ORDER BY entered_at DESC, and with tied timestamps each reader's pick was
// arbitrary — the grid could display one row's revenue while the calculation
// scheduler computed margin from ANOTHER row's, making margin ≠ revenue−cost
// by the grid's own numbers (off by 22k on the 500-product Test model).
// All readers now share an `id DESC` tiebreak, so the user-visible invariant
// holds: the grid's calc totals are consistent with the grid's input totals.
func TestSalesDemoTiedTimestampRowsStayConsistent(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	d.awaitCalc("margin", map[string]string{}, 760_000)

	// Three revenue rows at the same cell with an IDENTICAL entered_at —
	// exactly what one multi-row import transaction produces. Different
	// values, so an inconsistent pick is visible in the totals.
	combo := fmt.Sprintf(`{"%s":"UK","%s":"LAPTOP","%s":"Q1"}`, d.geoDim, d.prodDim, d.periodDim)
	ctx := context.Background()
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Rollback on any Fatalf below — a leaked open tx deadlocks the fixture's
	// pool.Close in cleanup. No-op after a successful Commit.
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, v := range []float64{111_111, 222_222, 333_333} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, $5, $6::uuid)`,
			d.modelID, d.revID, d.metric["revenue"], combo, v, d.westRepID); err != nil {
			t.Fatalf("insert tied row: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Recompute every calc metric from the new facts, like a real import does.
	sched := calculation.NewScheduler(logger.New("test"), calculation.NewStore(d.pool), nil)
	if err := sched.RecalcAffected(ctx, d.modelID, d.revID, []string{d.metric["revenue"], d.metric["cost"]}); err != nil {
		t.Fatalf("recalc: %v", err)
	}

	var g struct {
		Totals map[string]float64 `json:"totals"`
	}
	if _, raw := d.req("GET", "/api/grid?grid_id="+d.gridID, d.dev, nil); true {
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatalf("grid: %v\n%s", err, raw)
		}
	}
	rev, cost, margin := g.Totals[d.metric["revenue"]], g.Totals[d.metric["cost"]], g.Totals[d.metric["margin"]]
	if rev == 0 || cost == 0 {
		t.Fatalf("grid totals empty (revenue=%v cost=%v) — fixture changed", rev, cost)
	}
	if !nearly(margin, rev-cost) {
		t.Errorf("margin total = %v but revenue−cost = %v−%v = %v — the scheduler and the grid picked different rows among tied timestamps", margin, rev, cost, rev-cost)
	}
}

// TestSalesDemoTotalsOnlyMatchesFullRead is the KPI fast path: totals_only must
// return the SAME totals as a full read (both the unscoped fast path — per-
// metric input SUMs + '{}' calc rows — and the scoped path that reuses the
// per-combo re-resolution) while serializing NO cells.
func TestSalesDemoTotalsOnlyMatchesFullRead(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()

	read := func(q string) (map[string]float64, int) {
		var g struct {
			Cells  map[string]float64 `json:"cells"`
			Totals map[string]float64 `json:"totals"`
		}
		if _, raw := d.req("GET", "/api/grid?grid_id="+d.gridID+q, d.dev, nil); true {
			if err := json.Unmarshal(raw, &g); err != nil {
				t.Fatalf("grid%s: %v\n%s", q, err, raw)
			}
		}
		return g.Totals, len(g.Cells)
	}
	sameTotals := func(label string, full, only map[string]float64) {
		for id, want := range full {
			if got := only[id]; !nearly(got, want) {
				t.Errorf("%s: totals_only[%s]=%v, full read=%v", label, id, got, want)
			}
		}
	}

	// Unscoped (fast path): '{}' calc rows + grouped input sums.
	fullTotals, fullCells := read("")
	onlyTotals, onlyCells := read("&totals_only=1")
	if fullCells == 0 {
		t.Fatal("full read returned no cells — fixture changed")
	}
	if onlyCells != 0 {
		t.Errorf("totals_only returned %d cells, want 0", onlyCells)
	}
	sameTotals("unscoped", fullTotals, onlyTotals)

	// Scoped (re-resolution path): same totals, still no cells.
	sq := "&scope=" + url.QueryEscape(fmt.Sprintf(`{%q:%q}`, d.geoDim, "UK"))
	sFull, _ := read(sq)
	sOnly, sOnlyCells := read(sq + "&totals_only=1")
	if sOnlyCells != 0 {
		t.Errorf("scoped totals_only returned %d cells, want 0", sOnlyCells)
	}
	sameTotals("scoped", sFull, sOnly)
}

// setAccessRules replaces a user's whole rule set — the endpoint is a PUT, so
// a partial list silently revokes everything omitted.
func (d *salesDemo) setAccessRules(userID string, rules ...map[string]string) {
	d.t.Helper()
	list := make([]map[string]string, 0, len(rules))
	list = append(list, rules...)
	d.call("PUT", "/api/business-admin/users/"+userID+"/access-rules", d.admin, map[string]any{"rules": list})
}

func member(refID, access string) map[string]string {
	return map[string]string{"rule_type": "dimension_member", "ref_id": refID, "access": access}
}

// 2 — access rules for three business users.
//
// One plans their own region, one may look but not touch, one cannot see a
// region at all.
//
// The rule semantics are not symmetric, and this pins that rather than
// assuming it. Writing to a member is refused when the member itself carries
// read/hidden, OR when an ancestor is hidden — but an ancestor marked "read"
// does NOT make its children read-only (see CheckWrite in
// internal/writeguard). So "read" has to be granted on the members people
// actually plan at, while "hidden" can be granted once at the top.
func TestSalesDemoAccessRules(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()

	// Wes plans EMEA. The Americas are read-only for him, which has to be
	// stated on the leaves he could otherwise write to.
	d.setAccessRules(d.westRepID,
		member(d.geo["US"], "read"), member(d.geo["CA"], "read"),
	)
	// Eastin is the mirror image, so the two scopes are provably independent
	// rather than one permissive rule that happens to look right.
	d.setAccessRules(d.eastRepID,
		member(d.geo["UK"], "read"), member(d.geo["DE"], "read"),
	)
	// Reed may read EMEA and cannot see the Americas at all. "hidden" on the
	// parent is enough — that one does cascade.
	d.setAccessRules(d.roID,
		member(d.geo["UK"], "read"), member(d.geo["DE"], "read"),
		member(d.geo["AMER"], "hidden"),
	)

	t.Run("a planner writes their own region", func(t *testing.T) {
		if status, body := d.writeCell(d.westRep, "revenue", "UK", "LAPTOP", "Q1", 950_000); status != http.StatusOK {
			t.Fatalf("Wes writing UK: status %d, want 200\n%s", status, body)
		}
	})

	t.Run("and is refused outside it", func(t *testing.T) {
		status, body := d.writeCell(d.westRep, "revenue", "US", "LICENSE", "Q1", 1)
		if status != http.StatusForbidden {
			t.Errorf("Wes writing the Americas: status %d, want 403\n%s", status, body)
		}
	})

	t.Run("the mirrored scope is independent", func(t *testing.T) {
		if status, body := d.writeCell(d.eastRep, "revenue", "US", "LICENSE", "Q1", 420_000); status != http.StatusOK {
			t.Fatalf("Eastin writing the US: status %d, want 200\n%s", status, body)
		}
		if status, _ := d.writeCell(d.eastRep, "revenue", "UK", "LAPTOP", "Q1", 1); status != http.StatusForbidden {
			t.Errorf("Eastin wrote to EMEA, which is read-only for him: status %d", status)
		}
	})

	t.Run("read-only means read-only, but the grid still opens", func(t *testing.T) {
		if status, _ := d.writeCell(d.ro, "revenue", "UK", "LAPTOP", "Q1", 1); status != http.StatusForbidden {
			t.Error("Reed wrote a cell he only has read access to")
		}
		status, raw := d.req("GET", "/api/grid?grid_id="+d.gridID, d.ro, nil)
		if status != http.StatusOK {
			t.Fatalf("Reed opening the grid: status %d\n%s", status, raw)
		}
		if !bytes.Contains(raw, []byte(`"UK"`)) {
			t.Error("EMEA is readable for Reed but UK is missing from the grid")
		}
	})

	t.Run("hidden is absent from the members and from the totals", func(t *testing.T) {
		status, raw := d.req("GET", "/api/grid?grid_id="+d.gridID, d.ro, nil)
		if status != http.StatusOK {
			t.Fatalf("grid: status %d\n%s", status, raw)
		}
		// Absent entirely, not merely blanked — and the cascade means the
		// leaves go with the parent without rules of their own.
		for _, code := range []string{"US", "CA", "AMER"} {
			if bytes.Contains(raw, []byte(`"`+code+`"`)) {
				t.Errorf("%s is hidden from Reed but appears in the grid payload", code)
			}
		}

		// And absent from the arithmetic, which is the part that matters: if
		// the Americas leaked into his total he could subtract what he can see
		// and recover a number he was never allowed.
		var payload struct {
			Totals map[string]float64 `json:"totals"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode grid: %v", err)
		}
		got := payload.Totals[d.metric["revenue"]]
		if !nearly(got, 1_450_000) {
			t.Errorf("Reed's revenue total = %v, want 1450000 (UK 950k + DE 500k, no Americas)", got)
		}
	})
}

// 3 — workflow and triggers.
//
// A regional sign-off: the rep submits their region, the manager approves, and
// that region locks. The scoping is the part worth testing — submitting EMEA
// must not freeze the Americas, which is what makes a plan approvable region
// by region rather than all at once.
//
// The lock comes from the workflow's context_schema declaring a "Dimension
// member" variable; an instance whose context binds no member locks nothing,
// which is correct and is the trap a first attempt at this falls into.
func TestSalesDemoWorkflowAndTriggers(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	ctx := context.Background()

	defID := idOf(d.call("POST", "/api/developer/workflows?application_id="+d.appID, d.dev, map[string]any{
		"name": "Sales Plan Approval", "description": "Locks a region once its manager signs it off.",
		"trigger_event": "manual",
	}))

	// Route keys are what the runtime engine reads: "approve"/"reject" on an
	// approval step. Wrong keys stall the instance silently rather than error.
	d.call("PATCH", "/api/developer/workflows/"+defID, d.dev, map[string]any{
		"name": "Sales Plan Approval", "trigger_event": "manual",
		"steps": []map[string]any{
			{
				"id": "review", "name": "Regional Review", "type": "approval",
				"assignee_roles": []string{"business_admin"}, "required_comment": true,
				"routes": map[string]string{"approve": "end-approved", "reject": "end-rejected"},
			},
		},
		"context_schema": []map[string]any{
			{"key": "scope", "data_type": "Dimension member", "required": true, "dimension_id": d.geoDim},
			{"key": "revision_id", "data_type": "Text", "required": true},
		},
	})
	d.call("POST", "/api/developer/workflows/"+defID+"/publish", d.dev, nil)

	submit := func(persona, scope string) (instanceID, stepID string) {
		t.Helper()
		body := d.call("POST", "/api/workflow/instances", persona, map[string]any{
			"workflow_def_id": defID,
			"context":         map[string]string{"scope": scope, "model_id": d.modelID, "revision_id": d.revID},
		})
		instanceID, _ = body["instance_id"].(string)
		if instanceID == "" {
			t.Fatalf("no instance_id in %v", body)
		}
		if err := d.pool.QueryRow(ctx,
			`SELECT id::text FROM workflow.workflow_step WHERE instance_id=$1::uuid`, instanceID).Scan(&stepID); err != nil {
			t.Fatalf("find step: %v", err)
		}
		return
	}

	_, stepID := submit(d.westRep, "EMEA")

	t.Run("submitting a region locks that region", func(t *testing.T) {
		if status, body := d.writeCell(d.westRep, "revenue", "UK", "LAPTOP", "Q1", 1); status != http.StatusForbidden {
			t.Errorf("writing inside a submitted region: status %d, want 403\n%s", status, body)
		}
	})

	t.Run("and leaves every other region editable", func(t *testing.T) {
		// The whole point of scoping. If this fails, one region submitting
		// freezes the entire company.
		if status, body := d.writeCell(d.eastRep, "revenue", "US", "LICENSE", "Q1", 410_000); status != http.StatusOK {
			t.Errorf("writing outside the submitted region: status %d, want 200\n%s", status, body)
		}
	})

	t.Run("the step reaches its assignee and nobody else", func(t *testing.T) {
		status, raw := d.req("GET", "/api/tasks", d.admin, nil)
		if status != http.StatusOK {
			t.Fatalf("approver inbox: status %d\n%s", status, raw)
		}
		if !bytes.Contains(raw, []byte(stepID)) {
			t.Errorf("the pending step is not in the approver's inbox:\n%s", raw)
		}
		if _, repRaw := d.req("GET", "/api/tasks", d.ro, nil); bytes.Contains(repRaw, []byte(stepID)) {
			t.Error("a business_user sees a step assigned to business_admin")
		}
	})

	t.Run("a required comment is required, even to approve", func(t *testing.T) {
		if status, _ := d.req("POST", "/api/tasks/"+stepID+"/complete", d.admin,
			map[string]string{"decision": "approve", "comment": ""}); status != http.StatusBadRequest {
			t.Errorf("approving without the required comment: status %d, want 400", status)
		}
	})

	t.Run("approval keeps the region locked afterwards", func(t *testing.T) {
		if status, body := d.req("POST", "/api/tasks/"+stepID+"/complete", d.admin,
			map[string]string{"decision": "approve", "comment": "signed off"}); status != http.StatusOK {
			t.Fatalf("approve: status %d\n%s", status, body)
		}
		// The lock has to outlive the instance completing — the difference
		// between "is something running" and "was this approved".
		if status, _ := d.writeCell(d.westRep, "revenue", "UK", "LAPTOP", "Q1", 1); status != http.StatusForbidden {
			t.Error("an approved region accepted an edit")
		}
	})

	t.Run("an automation rule wires an event to this workflow", func(t *testing.T) {
		rule := d.call("POST", "/api/automation/rules", d.dev, map[string]any{
			"name": "Start approval when a plan is submitted", "trigger_type": "form_submit",
			"workflow_name": "Sales Plan Approval", "workflow_def_id": defID,
		})
		ruleID := idOf(rule)
		if ruleID == "" {
			t.Fatalf("no rule id in %v", rule)
		}
		// Rules fire from events (workflow.Store.DispatchEventRules), not from
		// a manual endpoint, so what is asserted here is the wiring: enabled,
		// and pointing at the workflow it names. The event path itself is
		// covered where an event exists to raise — see the form test.
		var enabled bool
		var boundDef string
		if err := d.pool.QueryRow(ctx,
			`SELECT enabled, workflow_def_id::text FROM workflow.automation_rule WHERE id=$1::uuid`, ruleID,
		).Scan(&enabled, &boundDef); err != nil {
			t.Fatalf("read rule: %v", err)
		}
		if !enabled {
			t.Error("a newly created rule is disabled")
		}
		if boundDef != defID {
			t.Errorf("rule points at %s, want the workflow it names (%s)", boundDef, defID)
		}
	})
}

// 4 — spreadsheet import, done by the business user who owns the numbers.
//
// The CSV is what someone pastes out of Excel: a column per dimension, a
// column per metric, member codes in the cells. Two things matter beyond "the
// numbers arrive" — that calculated metrics recompute from imported input, and
// that an import obeys the same access rules a typed cell does. An import that
// bypasses them is a much bigger hole than a cell write that does, because it
// arrives a thousand rows at a time.
func TestSalesDemoImportByBusinessUser(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()

	// Wes plans EMEA; the Americas are read-only for him.
	d.setAccessRules(d.westRepID,
		member(d.geo["US"], "read"), member(d.geo["CA"], "read"),
	)

	csv := func(rows ...string) string {
		out := "geography,product,period,units,revenue\n"
		for _, r := range rows {
			out += r + "\n"
		}
		return out
	}

	t.Run("a planner imports their own region", func(t *testing.T) {
		resp := d.call("POST", "/api/import/upload", d.westRep, map[string]any{
			"csv":         csv("UK,MONITOR,Q2,300,150000", "DE,MONITOR,Q2,120,90000"),
			"revision_id": d.revID,
			"import_mode": "replace",
		})
		if errRows, _ := resp["error_rows"].(float64); errRows != 0 {
			t.Fatalf("import reported %v error rows: %v", errRows, resp)
		}
		// valid_rows counts accepted CELLS, not CSV lines: two lines carrying
		// units and revenue apiece is four.
		if valid, _ := resp["valid_rows"].(float64); valid != 4 {
			t.Errorf("valid_rows = %v, want 4 (2 rows x 2 metrics)", valid)
		}
	})

	t.Run("imported input flows through the calculated metrics", func(t *testing.T) {
		// Revenue rises by 240k, so the totals every calc metric derives must
		// move with it — an import that lands facts but leaves calc_result
		// stale is the failure this catches.
		// Original margin 760k, plus 240k of new revenue at no recorded cost.
		d.awaitCalc("margin", map[string]string{}, 1_000_000)
	})

	t.Run("an import cannot reach a region the importer may only read", func(t *testing.T) {
		status, raw := d.req("POST", "/api/import/upload", d.westRep, map[string]any{
			"csv":         csv("US,MONITOR,Q2,999,999000"),
			"revision_id": d.revID,
			"import_mode": "replace",
		})
		// Either the request is refused outright or the row is rejected — both
		// are fine, silently writing it is not.
		if status == http.StatusOK {
			var resp map[string]any
			_ = json.Unmarshal(raw, &resp)
			if errRows, _ := resp["error_rows"].(float64); errRows == 0 {
				t.Errorf("an import wrote to a read-only region without complaint: %v", resp)
			}
		} else if status != http.StatusForbidden && status != http.StatusBadRequest {
			t.Errorf("import into a read-only region: status %d\n%s", status, raw)
		}

		// And the value must not be there either way.
		if v, ok := d.calcValue("revenue", map[string]string{
			d.geoDim: "US", d.prodDim: "MONITOR", d.periodDim: "Q2",
		}); ok && nearly(v, 999_000) {
			t.Error("the rejected row was written anyway")
		}
	})
}

// 5 — a form, its import, and the sync into the grid.
//
// Reps file adjustments on a form rather than typing into the plan; approved
// ones post into a metric and show up in the grid alongside everything else.
// The chain is form -> record -> mapping -> fact -> calculated total, and each
// link is somewhere a number can quietly stop moving.
func TestSalesDemoFormImportAndGridSync(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()

	formID := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "sales_adjustment", "label": "Sales Adjustment",
		"fields": []map[string]any{
			{"name": "geography", "label": "Region", "type": "dimension", "required": true, "dimension_id": d.geoDim},
			{"name": "product", "label": "Product", "type": "dimension", "required": true, "dimension_id": d.prodDim},
			{"name": "period", "label": "Period", "type": "dimension", "required": true, "dimension_id": d.periodDim},
			{"name": "amount", "label": "Adjustment", "type": "number", "required": true},
		},
	}))

	// Several adjustments can land on the same cell, so the mapping sums them
	// rather than letting the last one win.
	mappingID := idOf(d.call("POST", "/api/developer/form-integrations", d.dev, map[string]any{
		"form_id": formID, "grid_id": "", "name": "Post adjustments to revenue",
		"source_field": "amount", "target_metric_id": d.metric["revenue"],
		"aggregation": "sum", "posting_statuses": []string{"approved"},
		"dimension_mappings": map[string]string{
			d.geoDim: "geography", d.prodDim: "product", d.periodDim: "period",
		},
		"live_posting": true,
	}))
	if mappingID == "" {
		t.Fatal("no mapping id")
	}

	// A record posts when it BECOMES approved, not when it is created with a
	// status — creation then approval is the sequence the runtime watches.
	fileAdjustment := func(geo, prod, period string, amount float64, approve bool) {
		t.Helper()
		data := map[string]any{"geography": geo, "product": prod, "period": period, "amount": amount}
		rec := d.call("POST", "/api/forms/"+formID+"/records", d.westRep, map[string]any{"data": data})
		recID := idOf(rec)
		if recID == "" {
			t.Fatalf("no record id in %v", rec)
		}
		status := "draft"
		if approve {
			status = "approved"
		}
		d.call("PUT", "/api/records/"+recID, d.westRep, map[string]any{"data": data, "status": status})
	}

	// Deliberately a cell nobody has typed into. A form mapping posting to a
	// cell that ALSO carries a direct entry does not add to it — the posted
	// aggregate takes the cell — and mixing the two here would test that
	// interaction rather than the chain this case is about.
	t.Run("an approved record posts into the mapped metric", func(t *testing.T) {
		fileAdjustment("UK", "MONITOR", "Q3", 50_000, true)
		// Revenue was 1.9m across the seeded cells; the adjustment adds 50k.
		d.awaitCalc("margin", map[string]string{}, 810_000)
	})

	t.Run("two records against one cell sum rather than overwrite", func(t *testing.T) {
		fileAdjustment("UK", "MONITOR", "Q3", 25_000, true)
		// A mapping set to "sum" that let the newer record win would leave the
		// margin at 810k rather than 835k — the difference between adding an
		// adjustment and replacing every earlier one.
		d.awaitCalc("margin", map[string]string{}, 835_000)
	})

	t.Run("an unapproved record posts nothing", func(t *testing.T) {
		fileAdjustment("DE", "LAPTOP", "Q1", 999_000, false)
		// posting_statuses is ["approved"], so a draft must stay out of the
		// numbers entirely — a plan that counts drafts is worse than one that
		// counts nothing.
		d.awaitCalc("margin", map[string]string{}, 835_000)
	})

	// A real .xlsx, not a CSV standing in for one: the endpoint takes
	// xlsx_base64 and parses it with excelize, so uploading a spreadsheet is
	// the path a business user actually uses and the one worth exercising.
	t.Run("an Excel upload creates records but does not post them yet", func(t *testing.T) {
		xl := excelize.NewFile()
		defer xl.Close() //nolint:errcheck
		sheet := xl.GetSheetName(0)
		rows := [][]any{
			{"geography", "product", "period", "amount"},
			{"UK", "MONITOR", "Q2", 10_000},
			{"DE", "MONITOR", "Q2", 5_000},
		}
		for i, row := range rows {
			cell, _ := excelize.CoordinatesToCellName(1, i+1)
			if err := xl.SetSheetRow(sheet, cell, &row); err != nil {
				t.Fatalf("write xlsx row: %v", err)
			}
		}
		buf, err := xl.WriteToBuffer()
		if err != nil {
			t.Fatalf("serialise xlsx: %v", err)
		}

		resp := d.call("POST", "/api/forms/"+formID+"/import", d.westRep, map[string]any{
			"xlsx_base64": base64.StdEncoding.EncodeToString(buf.Bytes()),
		})
		if created, _ := resp["records_created"].(float64); created != 2 {
			t.Errorf("records_created = %v, want 2 (response: %v)", created, resp)
		}

		// Import carries no status, so the records arrive unapproved and the
		// mapping's posting_statuses of ["approved"] correctly ignores them.
		// A spreadsheet that posted on upload would be a way to write numbers
		// into the plan without anyone approving them.
		d.awaitCalc("margin", map[string]string{}, 835_000)
	})

	t.Run("approving the imported records posts them", func(t *testing.T) {
		rows, err := d.pool.Query(context.Background(),
			`SELECT id::text, data::text FROM runtime.form_record WHERE form_id=$1::uuid AND status <> 'approved'`, formID)
		if err != nil {
			t.Fatalf("list unapproved records: %v", err)
		}
		type rec struct{ id, data string }
		var pending []rec
		for rows.Next() {
			var r rec
			if err := rows.Scan(&r.id, &r.data); err != nil {
				t.Fatalf("scan record: %v", err)
			}
			pending = append(pending, r)
		}
		rows.Close()

		approved := 0
		for _, r := range pending {
			var data map[string]any
			if err := json.Unmarshal([]byte(r.data), &data); err != nil {
				t.Fatalf("decode record data: %v", err)
			}
			// The draft from the earlier case is meant to stay a draft; only
			// the two imported MONITOR/Q2 rows get approved here.
			if data["product"] != "MONITOR" {
				continue
			}
			d.call("PUT", "/api/records/"+r.id, d.westRep, map[string]any{"data": data, "status": "approved"})
			approved++
		}
		if approved != 2 {
			t.Fatalf("approved %d imported records, want 2", approved)
		}
		// 835k plus the two adjustments now that they are approved.
		d.awaitCalc("margin", map[string]string{}, 850_000)
	})
}
