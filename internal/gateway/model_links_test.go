package gateway

// Model links end to end through the real handler: a developer of both
// models links a grid of one application's model into another's, tests and
// runs it (the source read is the gateway's own /api/grid, calculated
// metrics included), and the rules hold — who may set one up, run it and
// switch each side, that a switched-off side stops every run, that the
// source switch follows the link into a new revision, and that a developer
// who loses the source model stops the link instead of reading through it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

type linkFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	srv  *httptest.Server
	ml   *ModelLinks
	run  *integration.Runner

	srcApp, srcModel, srcRev string
	tgtApp, tgtModel, tgtRev string
	foreignModel             string
	inbound                  string            // target grid id
	tgtMetric                map[string]string // target metric name -> id
	devID                    string
}

const (
	linkDev    = "ml-dev"    // developer of both applications
	linkTgtDev = "ml-tgtdev" // developer of the target application only
	linkSrcDev = "ml-srcdev" // developer of the source application only
	linkAdmin  = "ml-admin"  // the tenant's administrator
)

// req calls the gateway as persona in app (and its revision).
func (f *linkFixture) req(method, path, persona, app, rev string, body any) (int, []byte) {
	f.t.Helper()
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	r, _ := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, bytes.NewReader(buf))
	r.Header.Set("X-Dev-User", persona)
	r.Header.Set("Content-Type", "application/json")
	if app != "" {
		r.Header.Set("X-App-Id", app)
	}
	if rev != "" {
		r.Header.Set("X-Revision-Id", rev)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	out := new(bytes.Buffer)
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}

// ok calls and wants a 2xx, returning the answer's id.
func (f *linkFixture) ok(method, path, persona, app, rev string, body any) string {
	f.t.Helper()
	status, raw := f.req(method, path, persona, app, rev, body)
	if status < 200 || status >= 300 {
		f.t.Fatalf("%s %s as %s: %d %s", method, path, persona, status, raw)
	}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	id, _ := parsed["id"].(string)
	return id
}

// refused wants status, with want in the answer.
func (f *linkFixture) refused(what string, status int, raw []byte, wantStatus int, want string) {
	f.t.Helper()
	if status != wantStatus || !strings.Contains(string(raw), want) {
		f.t.Errorf("%s: got %d %s, want %d containing %q", what, status, raw, wantStatus, want)
	}
}

// setupLinkFixture builds the two models; provider, when set, answers the
// AI Developer's turns (aiTurn).
func setupLinkFixture(t *testing.T, provider ...providers.Provider) *linkFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	f := &linkFixture{t: t, pool: pool, tgtMetric: map[string]string{}}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Link Co', 'enterprise') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'W') RETURNING id::text`, cust)
	app := func(name, model string) (string, string, string) {
		a := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, $3, 'planning') RETURNING id::text`, ws, cust, name)
		m := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, a, model)
		r := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, m)
		exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, r, m)
		return a, m, r
	}
	f.srcApp, f.srcModel, f.srcRev = app("Sales", "Sales plan")
	f.tgtApp, f.tgtModel, f.tgtRev = app("Finance", "P&L")

	// Another tenant's model, whose id the developers above know.
	cust2 := q(`INSERT INTO core.customer (name, plan) VALUES ('Other Co', 'enterprise') RETURNING id::text`)
	ws2 := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'W2') RETURNING id::text`, cust2)
	app2 := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Theirs', 'planning') RETURNING id::text`, ws2, cust2)
	f.foreignModel = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Theirs') RETURNING id::text`, app2)

	person := func(sub, role string, apps ...string) string {
		uid := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1 || '@link.co', $1, $2::uuid) RETURNING id::text`, sub, cust)
		exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, $3::uuid)`, uid, role, ws)
		for _, a := range apps {
			exec(`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, uid, a)
		}
		return uid
	}
	f.devID = person(linkDev, "developer")
	person(linkTgtDev, "developer", f.tgtApp)
	person(linkSrcDev, "developer", f.srcApp)
	person(linkAdmin, "tenant_admin")

	t.Setenv("DEV_MODE", "true")
	// The handler as NewHandlerWithDeps assembles it, with a scripted AI
	// provider when one is given.
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true}
	if len(provider) > 0 {
		h.testProvider = provider[0]
	}
	h.plans = plan.NewEnforcer(h.db.Control())
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	api := appIDMiddleware(h.tenantRouting(h.delegatedReadGate(h.planGuard(mux))))
	f.ml = &ModelLinks{h: h, api: api}
	f.srv = httptest.NewServer(api)
	t.Cleanup(f.srv.Close)
	f.run = f.ml.runner(pool, logger.New("test"))
	f.build()
	return f
}

// build makes the two models over HTTP. Source: region (UK, DE) × revenue
// (input) and double (= revenue * 2) in grid "Sales". Target: region × two
// inputs in grid "Inbound".
func (f *linkFixture) build() {
	model := func(app, rev string, inputs []string, calcs map[string]string, gridName string) (string, map[string]string, string) {
		dim := f.ok("POST", "/api/developer/dimensions", linkDev, app, rev, map[string]any{"name": "region", "revision_id": rev, "dimension_type": "standard"})
		for _, c := range []string{"UK", "DE"} {
			f.ok("POST", "/api/developer/dimensions/"+dim+"/members", linkDev, app, rev, map[string]any{"code": c, "label": c})
		}
		grid := f.ok("POST", "/api/developer/grids", linkDev, app, rev, map[string]any{"name": gridName, "revision_id": rev})
		f.ok("POST", "/api/developer/grids/"+grid+"/dimensions/"+dim, linkDev, app, rev, nil)
		metrics := map[string]string{}
		add := func(name, formula string) {
			metrics[name] = f.ok("POST", "/api/developer/metrics", linkDev, app, rev, map[string]any{"name": name, "is_input": formula == "",
				"formula": formula, "revision_id": rev, "agg_rule": "sum", "format": "number"})
			f.ok("POST", "/api/developer/grids/"+grid+"/metrics/"+metrics[name], linkDev, app, rev, nil)
		}
		for _, n := range inputs {
			add(n, "")
		}
		for n, fm := range calcs {
			add(n, fm)
		}
		return dim, metrics, grid
	}
	srcDim, srcMetrics, _ := model(f.srcApp, f.srcRev, []string{"revenue"}, map[string]string{"double": "revenue * 2"}, "Sales")
	_, f.tgtMetric, f.inbound = model(f.tgtApp, f.tgtRev, []string{"rev_in", "dbl_in"}, nil, "Inbound")
	for code, v := range map[string]float64{"UK": 100, "DE": 200} {
		f.ok("POST", "/api/cells", linkDev, f.srcApp, f.srcRev, map[string]any{"model_id": f.srcModel, "metric_id": srcMetrics["revenue"],
			"revision_id": f.srcRev, "dim_codes": map[string]string{srcDim: code}, "value": v})
	}
	// The calculated metric is computed after each write: wait for it.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var v float64
		err := f.pool.QueryRow(context.Background(), `
			SELECT value FROM runtime.calc_result WHERE metric_id=$1::uuid AND dim_members=$2::jsonb ORDER BY calc_at DESC LIMIT 1`,
			srcMetrics["double"], fmt.Sprintf(`{%q: "DE"}`, srcDim)).Scan(&v)
		if err == nil && v == 400 {
			break
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("double at DE never reached 400 (last %v, %v)", v, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// linkConfig is a link from the source's Sales grid into Inbound.
func (f *linkFixture) linkConfig(sourceModel string) map[string]any {
	toNum := []map[string]any{{"kind": "to_number"}}
	return map[string]any{
		"kind": "rest_api/v1", "protocol": "model", "direction": "pull",
		"model":       map[string]any{"model_id": sourceModel, "grid": "Sales"},
		"target_type": "grid", "target_id": f.inbound, "import_mode": "replace",
		"auth": map[string]any{"type": "none"},
		"mapping": map[string]any{"fields": []map[string]any{
			{"source": "$.region", "target": "region"},
			{"source": "$.revenue", "target": "rev_in", "transforms": toNum},
			{"source": "$.double", "target": "dbl_in", "transforms": toNum},
		}},
	}
}

// runQueued runs the one queued model link run and returns its result.
func (f *linkFixture) runQueued() integration.Run {
	f.t.Helper()
	ran, err := f.ml.runNext(context.Background(), f.run, "test")
	if err != nil || !ran {
		f.t.Fatalf("no model link run claimed (%v)", err)
	}
	var run integration.Run
	if err := f.pool.QueryRow(context.Background(), `
		SELECT id::text, status, error_code, message, records_read, records_written
		FROM model.integration_run ORDER BY created_at DESC LIMIT 1`).
		Scan(&run.ID, &run.Status, &run.ErrorCode, &run.Message, &run.RecordsRead, &run.RecordsWritten); err != nil {
		f.t.Fatal(err)
	}
	return run
}

// inboundValue is the target grid's value of metric at region, as the
// developer reads it.
func (f *linkFixture) inboundValue(metric, region string) (float64, bool) {
	f.t.Helper()
	status, raw := f.req("GET", "/api/grid?grid_def_id="+f.inbound+"&revision_id="+f.tgtRev, linkDev, f.tgtApp, f.tgtRev, nil)
	if status != http.StatusOK {
		f.t.Fatalf("read Inbound: %d %s", status, raw)
	}
	var g struct {
		Cells map[string]float64 `json:"cells"`
	}
	_ = json.Unmarshal(raw, &g)
	v, ok := g.Cells[f.tgtMetric[metric]+":"+region]
	return v, ok
}

func TestModelLinkEndToEnd(t *testing.T) {
	f := setupLinkFixture(t)
	ctx := context.Background()
	create := func(persona string, cfg map[string]any) (int, []byte) {
		return f.req("POST", "/api/developer/integrations", persona, f.tgtApp, f.tgtRev,
			map[string]any{"type": "rest_api", "name": "Sales into P&L", "status": "draft", "config": cfg})
	}

	// ── Sources: only models of the tenant the caller builds ──
	var sources []modelLinkSource
	_, raw := f.req("GET", "/api/developer/model-link-sources", linkDev, f.tgtApp, f.tgtRev, nil)
	_ = json.Unmarshal(raw, &sources)
	if len(sources) != 1 || sources[0].ModelID != f.srcModel || len(sources[0].Grids) != 1 || sources[0].Grids[0].Name != "Sales" ||
		len(sources[0].Grids[0].Metrics) != 2 {
		t.Fatalf("sources as a developer of both: %s", raw)
	}
	_, raw = f.req("GET", "/api/developer/model-link-sources", linkTgtDev, f.tgtApp, f.tgtRev, nil)
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Errorf("sources as a developer of the target only: %s, want []", raw)
	}

	// ── Who may set one up ──
	status, raw := create(linkTgtDev, f.linkConfig(f.srcModel))
	f.refused("create as a developer of the target only", status, raw, http.StatusBadRequest, "developer of both models")
	status, raw = create(linkDev, f.linkConfig(f.foreignModel))
	f.refused("create reading another tenant's model", status, raw, http.StatusBadRequest, "not a model of this tenant")
	status, raw = create(linkDev, f.linkConfig(f.tgtModel))
	f.refused("create reading its own model", status, raw, http.StatusBadRequest, "reads another model")
	bad := f.linkConfig(f.srcModel)
	bad["model"] = map[string]any{"model_id": f.srcModel, "grid": "Nope"}
	status, raw = create(linkDev, bad)
	f.refused("create naming a missing grid", status, raw, http.StatusBadRequest, `has no grid`)
	id := f.ok("POST", "/api/developer/integrations", linkDev, f.tgtApp, f.tgtRev,
		map[string]any{"type": "rest_api", "name": "Sales into P&L", "status": "draft", "config": f.linkConfig(f.srcModel)})
	if n := 0; f.pool.QueryRow(ctx, `SELECT count(*) FROM model.integration_def WHERE id=$1::uuid AND source_model_id=$2::uuid`, id, f.srcModel).Scan(&n) != nil || n != 1 {
		t.Fatalf("source_model_id not kept")
	}

	// ── Test: only a developer of both; reads the source, writes nothing ──
	status, raw = f.req("POST", "/api/developer/integrations/"+id+"/test", linkTgtDev, f.tgtApp, f.tgtRev, nil)
	f.refused("test as a developer of the target only", status, raw, http.StatusBadRequest, "developer of both models")
	f.ok("POST", "/api/developer/integrations/"+id+"/test", linkDev, f.tgtApp, f.tgtRev, nil)
	if run := f.runQueued(); run.Status != "success" || run.RecordsRead != 2 {
		t.Fatalf("test run: %+v", run)
	}
	if _, ok := f.inboundValue("rev_in", "UK"); ok {
		t.Fatal("a test wrote into the target")
	}

	// ── Activate, run: the source's values, calculated ones included ──
	f.ok("PATCH", "/api/developer/integrations/"+id, linkDev, f.tgtApp, f.tgtRev, map[string]any{"status": "active"})
	status, raw = f.req("POST", "/api/integrations/"+id+"/run", linkTgtDev, f.tgtApp, f.tgtRev, nil)
	f.refused("run as a developer of the target only", status, raw, http.StatusBadRequest, "developer of both models")
	f.ok("POST", "/api/integrations/"+id+"/run", linkDev, f.tgtApp, f.tgtRev, nil)
	// Two records, a value of each of two metrics: four cells.
	if run := f.runQueued(); run.Status != "success" || run.RecordsRead != 2 || run.RecordsWritten != 4 {
		t.Fatalf("run: %+v", run)
	}
	// A second run reads the same values and leaves the target as it was.
	f.ok("POST", "/api/integrations/"+id+"/run", linkDev, f.tgtApp, f.tgtRev, nil)
	if run := f.runQueued(); run.Status != "success" {
		t.Fatalf("second run: %+v", run)
	}
	for _, c := range []struct {
		metric, region string
		want           float64
	}{{"rev_in", "UK", 100}, {"rev_in", "DE", 200}, {"dbl_in", "UK", 200}, {"dbl_in", "DE", 400}} {
		if v, ok := f.inboundValue(c.metric, c.region); !ok || v != c.want {
			t.Errorf("%s at %s = %v (%v), want %v", c.metric, c.region, v, ok, c.want)
		}
	}

	// ── The source side's switch ──
	status, raw = f.req("PATCH", "/api/developer/model-links/"+id, linkTgtDev, f.tgtApp, "", map[string]any{"source_enabled": false})
	f.refused("source switch by a developer of the target only", status, raw, http.StatusNotFound, "not found")
	var listed []modelLinkItem
	_, raw = f.req("GET", "/api/developer/model-links", linkSrcDev, f.srcApp, f.srcRev, nil)
	_ = json.Unmarshal(raw, &listed)
	if len(listed) != 1 || listed[0].ID != id || !listed[0].SourceEnabled || listed[0].LastRun == nil || listed[0].LastRun.Status != "success" {
		t.Fatalf("links reading the source, as its developer: %s", raw)
	}
	f.ok("PATCH", "/api/developer/model-links/"+id, linkSrcDev, f.srcApp, "", map[string]any{"source_enabled": false})
	status, raw = f.req("POST", "/api/integrations/"+id+"/run", linkDev, f.tgtApp, f.tgtRev, nil)
	f.refused("run while the source side is off", status, raw, http.StatusBadRequest, "source model's side")
	// A run queued before the switch-off (or by a schedule) reads nothing.
	st := integration.NewStore(f.pool)
	if _, err := st.Enqueue(ctx, id, "schedule", f.devID, false, nil); err != nil {
		t.Fatal(err)
	}
	if run := f.runQueued(); run.Status != "failed" || run.ErrorCode != integration.ErrCodeSwitchedOff {
		t.Fatalf("queued run with the source off: %+v", run)
	}

	// ── The source switch follows the link into a new revision ──
	newRev := f.ok("POST", "/api/developer/revisions", linkDev, f.tgtApp, f.tgtRev, map[string]any{"name": "Next", "source_revision_id": f.tgtRev})
	var copyID string
	var copyOn bool
	if err := f.pool.QueryRow(ctx, `
		SELECT c.id::text, c.source_enabled FROM model.integration_def c
		JOIN model.integration_def o ON o.link_id = c.link_id AND o.id = $1::uuid
		WHERE c.revision_id = $2::uuid AND c.source_model_id = $3::uuid`, id, newRev, f.srcModel).Scan(&copyID, &copyOn); err != nil {
		t.Fatalf("revision copy of the link: %v", err)
	}
	if copyOn {
		t.Error("the revision copy came back on although the source side switched the link off")
	}

	// ── The administrator holds both switches ──
	var adminList []modelLinkItem
	_, raw = f.req("GET", "/api/admin/model-links", linkAdmin, "", "", nil)
	_ = json.Unmarshal(raw, &adminList)
	if len(adminList) != 2 {
		t.Fatalf("admin list: %s, want the link and its revision copy", raw)
	}
	f.ok("PATCH", "/api/admin/model-links/"+id, linkAdmin, "", "", map[string]any{"source_enabled": true})
	if err := f.pool.QueryRow(ctx, `SELECT source_enabled FROM model.integration_def WHERE id=$1::uuid`, copyID).Scan(&copyOn); err != nil || !copyOn {
		t.Errorf("the source switch did not reach the revision copy (%v)", err)
	}
	f.ok("PATCH", "/api/admin/model-links/"+id, linkAdmin, "", "", map[string]any{"enabled": false})
	status, raw = f.req("POST", "/api/integrations/"+id+"/run", linkDev, f.tgtApp, f.tgtRev, nil)
	f.refused("run while the link's own side is off", status, raw, http.StatusBadRequest, "disabled")
	f.ok("PATCH", "/api/developer/integrations/"+id, linkTgtDev, f.tgtApp, f.tgtRev, map[string]any{"enabled": true})
	var audits int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type='integration.switched' AND resource_id=$1`, id).Scan(&audits)
	if audits != 3 {
		t.Errorf("integration.switched audit events = %d, want 3", audits)
	}

	// ── A developer who loses the source model stops the link ──
	if _, err := st.Enqueue(ctx, id, "schedule", f.devID, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, f.devID, f.tgtApp); err != nil {
		t.Fatal(err)
	}
	if run := f.runQueued(); run.Status != "failed" || run.ErrorCode != integration.ErrCodeAuth || !strings.Contains(run.Message, "no longer a developer of both models") {
		t.Fatalf("run for a developer who lost the source: %+v", run)
	}
}
