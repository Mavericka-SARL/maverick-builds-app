// A restricted user reading a NON-active revision must see it with the same
// rules applied as the active one. Rules store raw member / metric UUIDs and
// activation re-points them at the new revision's rows
// (remapAccessRulesToRevision), so every read and write path resolves them
// by lineage (migration 099: the lineage_id a row shares with its copies in
// every revision) against the revision actually requested (writeguard.RulesForRevision). Before that, the
// old revision's members matched no rule and it was served unrestricted.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// setupOldRevisionFixture builds, over HTTP, one flat region dimension
// (UK, DE, US), a Plan grid on it with input metrics revenue and cost and
// the calculated dbl = revenue * 2, and two chart widgets on that grid:
// one plotting revenue and dbl, one plotting cost.
// Revenue: UK 100, DE 200, US 300; cost: UK 10, DE 20, US 30.
func setupOldRevisionFixture(t *testing.T) (f *restrictedFixture, revWidget, costWidget string) {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	f = &restrictedFixture{t: t, pool: pool, dev: "or-dev", admin: "or-admin", viewer: "or-viewer",
		members: map[string]string{}, metric: map[string]string{}, dims: map[string][]string{}}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('OR Co', 'enterprise') RETURNING id::text`)
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

	dims := "/api/developer/dimensions"
	f.region = f.dev_("POST", dims, map[string]any{"name": "region", "revision_id": f.revID, "dimension_type": "standard"})
	for _, c := range []string{"UK", "DE", "US"} {
		f.members[c] = f.dev_("POST", dims+"/"+f.region+"/members", map[string]any{"code": c, "label": c})
	}
	f.plan = f.dev_("POST", "/api/developer/grids", map[string]any{"name": "Plan", "revision_id": f.revID})
	f.dev_("POST", "/api/developer/grids/"+f.plan+"/dimensions/"+f.region, nil)
	for _, m := range []struct{ name, text string }{{"revenue", ""}, {"cost", ""}, {"dbl", "revenue * 2"}} {
		f.metric[m.name] = f.dev_("POST", "/api/developer/metrics", map[string]any{"name": m.name, "is_input": m.text == "",
			"formula": m.text, "revision_id": f.revID, "agg_rule": "sum", "format": "number"})
		f.dev_("POST", "/api/developer/grids/"+f.plan+"/metrics/"+f.metric[m.name], nil)
	}
	for code, v := range map[string]float64{"UK": 100, "DE": 200, "US": 300} {
		for metric, scale := range map[string]float64{"revenue": 1, "cost": 0.1} {
			f.dev_("POST", "/api/cells", map[string]any{"model_id": f.modelID, "metric_id": f.metric[metric], "revision_id": f.revID,
				"dim_codes": map[string]string{f.region: code}, "value": v * scale})
		}
	}
	f.loadMetricDims()
	f.await("dbl", map[string]string{f.region: "US"}, 600)
	f.await("dbl", map[string]string{f.region: "DE"}, 400)
	f.await("dbl", map[string]string{f.region: "UK"}, 200)

	dash := f.dev_("POST", "/api/developer/dashboards", map[string]any{"name": "D", "revision_id": f.revID})
	revWidget = f.chartWidget(dash, f.region, []string{"revenue", "dbl"}, map[string]string{})
	costWidget = f.chartWidget(dash, f.region, []string{"cost"}, map[string]string{})
	return f, revWidget, costWidget
}

// restrictViewer gives the viewer US hidden, DE read-only and the cost
// metric hidden — through the business admin's own endpoint.
func restrictViewer(f *restrictedFixture) {
	f.t.Helper()
	f.call("PUT", "/api/business-admin/users/"+f.viewerID+"/access-rules", f.admin, map[string]any{"rules": []map[string]string{
		{"rule_type": "dimension_member", "ref_id": f.members["US"], "access": "hidden"},
		{"rule_type": "dimension_member", "ref_id": f.members["DE"], "access": "read"},
		{"rule_type": "metric", "ref_id": f.metric["cost"], "access": "hidden"},
	}})
}

// newRevisionAndActivate copies the active revision and activates the copy,
// as the developer, returning the new revision's ID.
func newRevisionAndActivate(f *restrictedFixture) string {
	f.t.Helper()
	revB := f.dev_("POST", "/api/developer/revisions", map[string]any{"name": "Next", "source_revision_id": f.revID})
	if revB == "" {
		f.t.Fatal("create revision: no id")
	}
	f.dev_("PUT", "/api/developer/revisions/"+revB+"/activate", nil)
	return revB
}

type oldRevGrid struct {
	Cells       map[string]float64 `json:"cells"`
	Totals      map[string]float64 `json:"totals"`
	AccessRules gridAccessRules    `json:"access_rules"`
}

func (f *restrictedFixture) gridAt(persona, query string) oldRevGrid {
	f.t.Helper()
	status, raw := f.req("GET", "/api/grid?"+query, persona, nil)
	if status != http.StatusOK {
		f.t.Fatalf("grid %s as %s: %d %s", query, persona, status, raw)
	}
	var g oldRevGrid
	if err := json.Unmarshal(raw, &g); err != nil {
		f.t.Fatalf("grid: %v", err)
	}
	return g
}

// assertRestrictedGrid checks a grid read of revision rev (whose member and
// metric IDs are memberIDs / metricIDs) applies US hidden, DE read-only and
// cost hidden.
func assertRestrictedGrid(f *restrictedFixture, label string, g oldRevGrid, memberIDs, metricIDs map[string]string, totalsOnly bool) {
	f.t.Helper()
	for k, v := range g.Cells {
		if strings.HasSuffix(k, ":US") {
			f.t.Errorf("%s: hidden member US served: %s = %v", label, k, v)
		}
		if strings.HasPrefix(k, metricIDs["cost"]) {
			f.t.Errorf("%s: hidden metric cost served: %s = %v", label, k, v)
		}
	}
	if !totalsOnly {
		if v, ok := g.Cells[metricIDs["revenue"]+":UK"]; !ok || !nearly(v, 100) {
			f.t.Errorf("%s: revenue UK = %v (present %v), want 100", label, v, ok)
		}
	}
	if v, ok := g.Totals[metricIDs["revenue"]]; !ok || !nearly(v, 300) {
		f.t.Errorf("%s: revenue total = %v (present %v), want 300 (UK + DE, US hidden)", label, v, ok)
	}
	if v, ok := g.Totals[metricIDs["cost"]]; ok {
		f.t.Errorf("%s: hidden metric cost has a total %v", label, v)
	}
	if v, ok := g.Totals[metricIDs["dbl"]]; ok && !nearly(v, 600) {
		f.t.Errorf("%s: dbl total = %v, want withheld or 600 (never the 1200 that includes US)", label, v)
	}
	if !totalsOnly {
		if got := g.AccessRules.DimMembers[memberIDs["DE"]]; got != "read" {
			f.t.Errorf("%s: DE access = %q in this revision, want read", label, got)
		}
		if got := g.AccessRules.Metrics[metricIDs["cost"]]; got != "hidden" {
			f.t.Errorf("%s: cost access = %q in this revision, want hidden", label, got)
		}
	}
}

// revisionIDs returns member code -> ID and metric name -> ID in rev.
func (f *restrictedFixture) revisionIDs(rev string) (members, metrics map[string]string) {
	f.t.Helper()
	ctx := context.Background()
	members, metrics = map[string]string{}, map[string]string{}
	rows, err := f.pool.Query(ctx, `
		SELECT m.code, m.id::text FROM model.dimension_member m
		JOIN model.dimension_def d ON d.id = m.dimension_id
		WHERE d.revision_id = $1::uuid AND d.name = 'region'`, rev)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var c, id string
		if err := rows.Scan(&c, &id); err != nil {
			f.t.Fatal(err)
		}
		members[c] = id
	}
	rows.Close()
	rows, err = f.pool.Query(ctx, `SELECT name, id::text FROM model.metric_def WHERE revision_id = $1::uuid`, rev)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var n, id string
		if err := rows.Scan(&n, &id); err != nil {
			f.t.Fatal(err)
		}
		metrics[n] = id
	}
	rows.Close()
	return members, metrics
}

func TestRestrictedUserReadsOldRevisionRestricted(t *testing.T) {
	f, widget, costWidget := setupOldRevisionFixture(t)
	revA := f.revID
	restrictViewer(f)

	// The active revision, before anything changes: the baseline.
	assertRestrictedGrid(f, "active A", f.gridAt(f.viewer, "grid_def_id="+f.plan), f.members, f.metric, false)

	revB := newRevisionAndActivate(f)
	membersB, metricsB := f.revisionIDs(revB)
	var gridB string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id::text FROM model.grid_def WHERE revision_id=$1::uuid AND name='Plan'`, revB).Scan(&gridB); err != nil {
		t.Fatal(err)
	}
	// The rules now point at revision B's rows — the precondition of the leak.
	var onA int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM identity.user_access_rule WHERE user_id=$1::uuid AND ref_id = ANY($2::text[])`,
		f.viewerID, []string{f.members["US"], f.members["DE"], f.metric["cost"]}).Scan(&onA); err != nil {
		t.Fatal(err)
	}
	if onA != 0 {
		t.Fatalf("%d rules still point at revision A after activation; the reproduction needs them remapped", onA)
	}

	// The active revision B behaves as before.
	assertRestrictedGrid(f, "active B", f.gridAt(f.viewer, "grid_def_id="+gridB), membersB, metricsB, false)

	// The OLD revision A, requested explicitly.
	oldQ := "grid_def_id=" + f.plan + "&revision_id=" + revA
	assertRestrictedGrid(f, "old A grid", f.gridAt(f.viewer, oldQ), f.members, f.metric, false)
	assertRestrictedGrid(f, "old A totals_only", f.gridAt(f.viewer, oldQ+"&totals_only=1"), f.members, f.metric, true)
	assertRestrictedGrid(f, "old A without grid", f.gridAt(f.viewer, "revision_id="+revA), f.members, f.metric, false)

	// The developer (no rules) still sees everything in A.
	if g := f.gridAt(f.dev, oldQ); !nearly(g.Totals[f.metric["revenue"]], 600) {
		t.Errorf("developer revenue total in A = %v, want 600", g.Totals[f.metric["revenue"]])
	}

	// Grid export of revision A.
	status, raw := f.req("GET", "/api/grid/export?format=csv&"+oldQ, f.viewer, nil)
	if status != http.StatusOK {
		t.Fatalf("export: %d %s", status, raw)
	}
	csv := string(raw)
	if strings.Contains(csv, "US") || strings.Contains(csv, "300") {
		t.Errorf("export of revision A serves hidden member US:\n%s", csv)
	}
	if strings.Contains(strings.ToLower(csv), "cost") {
		t.Errorf("export of revision A serves hidden metric cost:\n%s", csv)
	}
	if !strings.Contains(csv, "UK") || !strings.Contains(csv, "100") {
		t.Errorf("export of revision A lost the visible rows:\n%s", csv)
	}

	// Chart-data on the revision-A dashboard widget (charts resolve in their
	// grid's revision).
	c := f.chartPoints(f.viewer, widget)
	if v := c["revenue"]["US"]; v != nil {
		t.Errorf("chart on revision A plots hidden US revenue %v", *v)
	}
	if v := c["dbl"]["US"]; v != nil {
		t.Errorf("chart on revision A plots hidden US dbl %v", *v)
	}
	if v := c["revenue"]["UK"]; v == nil || !nearly(*v, 100) {
		t.Errorf("chart on revision A lost revenue UK: %v", v)
	}

	if status, raw := f.req("POST", "/api/dashboard-widgets/"+costWidget+"/chart-data", f.viewer,
		map[string]any{"context": map[string]string{}}); status != http.StatusForbidden {
		t.Errorf("chart of the hidden metric cost on revision A: status %d, want 403\n%.300s", status, raw)
	}

	// /api/metrics of revision A.
	status, raw = f.req("GET", "/api/metrics?revision_id="+revA, f.viewer, nil)
	if status != http.StatusOK {
		t.Fatalf("metrics: %d %s", status, raw)
	}
	var ms []metricRow
	if err := json.Unmarshal(raw, &ms); err != nil {
		t.Fatalf("metrics: %v %s", err, raw)
	}
	names := map[string]bool{}
	for _, m := range ms {
		names[m.Name] = true
	}
	if names["cost"] || !names["revenue"] {
		t.Errorf("/api/metrics of revision A: %v, want revenue listed and cost hidden", names)
	}

	// Cell writes into revision A: the read-only DE and the hidden US stay
	// protected, as does the hidden metric; UK stays writable.
	write := func(metric, code string) int {
		t.Helper()
		status, _ := f.req("POST", "/api/cells", f.viewer, map[string]any{"model_id": f.modelID, "metric_id": f.metric[metric],
			"revision_id": revA, "dim_codes": map[string]string{f.region: code}, "value": 999})
		return status
	}
	for _, w := range []struct{ metric, code string }{{"revenue", "DE"}, {"revenue", "US"}, {"cost", "UK"}} {
		if s := write(w.metric, w.code); s < 400 {
			t.Errorf("viewer write of %s at %s into revision A: status %d, want refused", w.metric, w.code, s)
		}
	}
	if s := write("revenue", "UK"); s >= 300 {
		t.Errorf("viewer write of revenue at UK into revision A: status %d, want accepted", s)
	}
}

// TestHiddenMetricOnOldRevision: a metric-only rule (no member rules) hides
// the metric in an older revision from the grid, chart-data and
// /api/metrics.
func TestHiddenMetricOnOldRevision(t *testing.T) {
	f, revWidget, costWidget := setupOldRevisionFixture(t)
	revA := f.revID
	f.call("PUT", "/api/business-admin/users/"+f.viewerID+"/access-rules", f.admin, map[string]any{"rules": []map[string]string{
		{"rule_type": "metric", "ref_id": f.metric["revenue"], "access": "hidden"},
	}})
	newRevisionAndActivate(f)

	g := f.gridAt(f.viewer, "grid_def_id="+f.plan+"&revision_id="+revA)
	for k, v := range g.Cells {
		if strings.HasPrefix(k, f.metric["revenue"]) {
			t.Errorf("old revision grid serves hidden metric revenue: %s = %v", k, v)
		}
	}
	if v, ok := g.Totals[f.metric["revenue"]]; ok {
		t.Errorf("old revision grid serves hidden metric revenue total %v", v)
	}
	if v, ok := g.Cells[f.metric["cost"]+":US"]; !ok || !nearly(v, 30) {
		t.Errorf("old revision grid: cost US = %v (present %v), want 30 (no member rules)", v, ok)
	}
	if status, raw := f.req("POST", "/api/dashboard-widgets/"+revWidget+"/chart-data", f.viewer,
		map[string]any{"context": map[string]string{}}); status != http.StatusForbidden {
		t.Errorf("old revision chart of hidden metric revenue: status %d, want 403\n%.300s", status, raw)
	}
	if v := f.chartPoints(f.viewer, costWidget)["cost"]["US"]; v == nil || !nearly(*v, 30) {
		t.Errorf("old revision chart: cost US = %v, want 30 (no member rules)", v)
	}
	_, raw := f.req("GET", "/api/metrics?revision_id="+revA, f.viewer, nil)
	if strings.Contains(string(raw), `"revenue"`) {
		t.Errorf("/api/metrics of old revision lists hidden metric revenue: %s", raw)
	}
	status, _ := f.req("POST", "/api/cells", f.viewer, map[string]any{"model_id": f.modelID, "metric_id": f.metric["revenue"],
		"revision_id": revA, "dim_codes": map[string]string{f.region: "UK"}, "value": 1})
	if status < 400 {
		t.Errorf("write of hidden metric revenue into old revision: status %d, want refused", status)
	}
}
