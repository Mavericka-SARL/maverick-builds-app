package gateway

// The chat connector (mcp.go) end to end on the sales demo model: a real MCP
// client calls /mcp as each persona, and every answer is checked against
// what the same person reads through the REST routes — the connector must
// never read more, and never report a different slice than it was asked for.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// mcpClient is one host's connection, as one person.
type mcpClient struct {
	t  *testing.T
	cs *sdk.ClientSession
}

func connectMCP(t *testing.T, baseURL, token string) *mcpClient {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test-host", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint:   baseURL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token}, Timeout: time.Minute},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &mcpClient{t: t, cs: cs}
}

// call calls tool and returns its structured result, or the tool error.
func (c *mcpClient) call(tool string, args map[string]any) (map[string]any, string) {
	c.t.Helper()
	res, err := c.cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		c.t.Fatalf("%s: protocol error: %v", tool, err)
	}
	if res.IsError {
		var texts []string
		for _, ct := range res.Content {
			if tc, ok := ct.(*sdk.TextContent); ok {
				texts = append(texts, tc.Text)
			}
		}
		return nil, strings.Join(texts, " ")
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%s: decode: %v", tool, err)
	}
	return out, ""
}

// must calls tool and fails on a tool error.
func (c *mcpClient) must(tool string, args map[string]any) map[string]any {
	c.t.Helper()
	out, errText := c.call(tool, args)
	if errText != "" {
		c.t.Fatalf("%s: %s", tool, errText)
	}
	return out
}

type connectorFixture struct {
	*salesDemo
	url string
}

func setupConnector(t *testing.T) *connectorFixture {
	t.Helper()
	d := setupSalesDemo(t)
	d.seedFacts()
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), d.pool, nil, Deps{
		MCP: MCPConfig{Enabled: true, ResourceURL: "http://connector.test/mcp"},
	}))
	t.Cleanup(srv.Close)
	return &connectorFixture{salesDemo: d, url: srv.URL}
}

// as connects as persona (a keycloak subject), through the dev stack's
// stand-in token.
func (f *connectorFixture) as(persona string) *mcpClient {
	return connectMCP(f.t, f.url, "dev:"+persona)
}

func (f *connectorFixture) ctxArgs(extra map[string]any) map[string]any {
	args := map[string]any{"application_id": f.appID, "model_id": f.modelID}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// restTotals is persona's own /api/grid totals for the grid at scope.
func (f *connectorFixture) restTotals(persona string, scope map[string]string) map[string]float64 {
	f.t.Helper()
	q := url.Values{"grid_def_id": {f.gridID}, "totals_only": {"1"}}
	if len(scope) > 0 {
		b, _ := json.Marshal(scope)
		q.Set("scope", string(b))
	}
	status, raw := f.req("GET", "/api/grid?"+q.Encode(), persona, nil)
	if status != http.StatusOK {
		f.t.Fatalf("REST grid totals as %s: %d %s", persona, status, raw)
	}
	var g struct {
		Totals map[string]float64 `json:"totals"`
	}
	_ = json.Unmarshal(raw, &g)
	return g.Totals
}

func valueOf(t *testing.T, v any) (float64, string) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not a value: %#v", v)
	}
	state, _ := m["state"].(string)
	n, _ := m["value"].(float64)
	return n, state
}

func codesOf(rows []any, key string) []string {
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprint(r.(map[string]any)[key]))
	}
	return out
}

func TestMCPConnectorReadsAsThePerson(t *testing.T) {
	f := setupConnector(t)
	// Reed cannot see the Americas at all.
	f.setAccessRules(f.roID, member(f.geo["AMER"], "hidden"))
	reed := f.as(f.ro)
	dev := f.as(f.dev)

	t.Run("every tool but write_cells is a read", func(t *testing.T) {
		list, err := reed.cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range list.Tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		want := []string{"compare_grid", "describe_dashboard", "describe_source", "describe_workflow", "get_connection_access",
			"list_dashboards", "list_members", "list_models", "list_sources", "list_workflows", "query_grid", "render_chart",
			"render_report", "write_cells"}
		if !slices.Equal(names, want) {
			t.Errorf("tools = %v, want the grid tools only %v", names, want)
		}
		for _, tool := range list.Tools {
			a := tool.Annotations
			if tool.Name == "write_cells" {
				// A host asks the person before a call that changes data.
				if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
					t.Errorf("write_cells is not annotated as a write that overwrites")
				}
				continue
			}
			if a == nil || !a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
				t.Errorf("%s is not annotated as a non-destructive read", tool.Name)
			}
		}
	})

	t.Run("models and sources are the person's", func(t *testing.T) {
		models := reed.must("list_models", nil)["models"].([]any)
		found := false
		for _, m := range models {
			if m.(map[string]any)["model_id"] == f.modelID {
				found = true
			}
		}
		if !found {
			t.Fatalf("list_models lacks the model Reed opens: %v", models)
		}
		src := reed.must("list_sources", f.ctxArgs(nil))
		if grids := src["grids"].([]any); len(grids) != 1 || grids[0].(map[string]any)["id"] != f.gridID {
			t.Errorf("grids = %v, want the one sales grid", grids)
		}
	})

	t.Run("hidden members are absent", func(t *testing.T) {
		page := reed.must("list_members", f.ctxArgs(map[string]any{"grid_id": f.gridID, "dimension_id": f.geoDim}))
		codes := codesOf(page["members"].([]any), "code")
		for _, hidden := range []string{"AMER", "US", "CA"} {
			if slices.Contains(codes, hidden) {
				t.Errorf("Reed is listed %s: %v", hidden, codes)
			}
		}
		if !slices.Contains(codes, "UK") {
			t.Errorf("UK missing for Reed: %v", codes)
		}
		all := codesOf(dev.must("list_members", f.ctxArgs(map[string]any{"grid_id": f.gridID, "dimension_id": f.geoDim}))["members"].([]any), "code")
		if !slices.Contains(all, "US") {
			t.Errorf("the developer is not listed US: %v", all)
		}
	})

	t.Run("a hidden filter is refused exactly like an unknown one", func(t *testing.T) {
		_, hiddenErr := reed.call("query_grid", f.ctxArgs(map[string]any{"grid_id": f.gridID, "filters": map[string]string{f.geoDim: "US"}}))
		_, unknownErr := reed.call("query_grid", f.ctxArgs(map[string]any{"grid_id": f.gridID, "filters": map[string]string{f.geoDim: "ZZ"}}))
		if hiddenErr == "" || unknownErr == "" {
			t.Fatalf("a filter on a member Reed cannot see was served: hidden=%q unknown=%q", hiddenErr, unknownErr)
		}
		if strings.Replace(hiddenErr, `"US"`, `"ZZ"`, 1) != unknownErr {
			t.Errorf("hidden and unknown members answer differently:\n%s\n%s", hiddenErr, unknownErr)
		}
	})

	t.Run("totals are the person's own engine totals", func(t *testing.T) {
		revenue := f.metric["revenue"]
		res := reed.must("query_grid", f.ctxArgs(map[string]any{"grid_id": f.gridID, "metric_ids": []string{revenue}}))
		got, state := valueOf(t, res["rows"].([]any)[0].(map[string]any)["value"])
		want := f.restTotals(f.ro, nil)[revenue]
		if state != "ok" || !nearly(got, want) {
			t.Errorf("Reed's revenue = %v (%s), REST says %v", got, state, want)
		}
		if devTotal := f.restTotals(f.dev, nil)[revenue]; !(want < devTotal) {
			t.Errorf("Reed's total %v is not below the developer's %v: the Americas were not left out", want, devTotal)
		}
	})

	t.Run("a breakdown has only visible members, each the engine's scoped total", func(t *testing.T) {
		revenue := f.metric["revenue"]
		res := reed.must("query_grid", f.ctxArgs(map[string]any{
			"grid_id": f.gridID, "metric_ids": []string{revenue}, "group_by": f.geoDim, "leaves_only": true,
		}))
		rows := res["rows"].([]any)
		codes := codesOf(rows, "member_code")
		slices.Sort(codes)
		if !slices.Equal(codes, []string{"DE", "UK"}) {
			t.Fatalf("Reed's leaf breakdown = %v, want DE and UK only", codes)
		}
		for _, r := range rows {
			row := r.(map[string]any)
			code := row["member_code"].(string)
			got, state := valueOf(t, row[revenue])
			want := f.restTotals(f.ro, map[string]string{f.geoDim: code})[revenue]
			if state != "ok" || !nearly(got, want) {
				t.Errorf("%s: breakdown %v (%s), scoped total %v", code, got, state, want)
			}
		}
	})

	t.Run("a metric hidden from the person is not available", func(t *testing.T) {
		f.setAccessRules(f.roID, member(f.geo["AMER"], "hidden"),
			map[string]string{"rule_type": "metric", "ref_id": f.metric["cost"], "access": "hidden"})
		t.Cleanup(func() { f.setAccessRules(f.roID, member(f.geo["AMER"], "hidden")) })
		desc := reed.must("describe_source", f.ctxArgs(map[string]any{"source_id": f.gridID}))
		for _, m := range desc["metrics"].([]any) {
			if m.(map[string]any)["id"] == f.metric["cost"] {
				t.Error("describe_source lists a hidden metric")
			}
		}
		if _, errText := reed.call("query_grid", f.ctxArgs(map[string]any{"grid_id": f.gridID, "metric_ids": []string{f.metric["cost"]}})); !strings.Contains(errText, "not available") {
			t.Errorf("querying a hidden metric: %q", errText)
		}
	})

	t.Run("a context the person may not open reads nothing", func(t *testing.T) {
		ctx := context.Background()
		var cust2, app2, model2 string
		_ = f.pool.QueryRow(ctx, `INSERT INTO core.customer (name, plan) VALUES ('Other Co', 'enterprise') RETURNING id::text`).Scan(&cust2)
		_ = f.pool.QueryRow(ctx, `INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'Other app', 'planning') RETURNING id::text`, cust2).Scan(&app2)
		_ = f.pool.QueryRow(ctx, `INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Other model') RETURNING id::text`, app2).Scan(&model2)
		for name, args := range map[string]map[string]any{
			"another tenant's model":              {"application_id": app2, "model_id": model2},
			"another application's model":         {"application_id": f.appID, "model_id": model2},
			"a model under the wrong application": {"application_id": app2, "model_id": f.modelID},
		} {
			if _, errText := reed.call("list_sources", args); !strings.HasPrefix(errText, "not_found") {
				t.Errorf("%s: %q, want not_found", name, errText)
			}
		}
	})

	t.Run("only the active revision is read", func(t *testing.T) {
		var draft string
		_ = f.pool.QueryRow(context.Background(), `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Draft') RETURNING id::text`, f.modelID).Scan(&draft)
		if _, errText := reed.call("list_sources", f.ctxArgs(map[string]any{"revision_id": draft})); !strings.Contains(errText, "active revision") {
			t.Errorf("a draft revision: %q", errText)
		}
		if _, errText := reed.call("list_sources", f.ctxArgs(map[string]any{"revision_id": f.revID})); errText != "" {
			t.Errorf("the active revision named explicitly: %q", errText)
		}
	})
}

func TestMCPConnectorStopsWhenAccessEnds(t *testing.T) {
	f := setupConnector(t)
	reed := f.as(f.ro)
	reed.must("list_models", nil)
	if _, err := f.pool.Exec(context.Background(), `UPDATE identity.user SET disabled_at = now() WHERE id = $1::uuid`, f.roID); err != nil {
		t.Fatal(err)
	}
	if _, errText := reed.call("list_models", nil); !strings.HasPrefix(errText, "unauthorized") {
		t.Errorf("a disabled account's next read: %q, want unauthorized", errText)
	}
}

func TestDelegatedReadGate(t *testing.T) {
	for _, path := range []string{"/api/grid", "/api/grid/series", "/api/dashboards", "/api/dashboards/abc", "/api/folders",
		"/api/workflow/definitions", "/api/workflow/definitions/abc"} {
		if !delegatedReadAllowed("GET", path) {
			t.Errorf("the allowlisted read %s is refused", path)
		}
	}
	for _, c := range [][2]string{
		{"GET", "/api/forms/abc/records"}, {"POST", "/api/dashboard-widgets/x/chart-data"}, {"GET", "/api/workflow/my-history"},
		{"GET", "/api/tasks"}, {"GET", "/api/notifications"}, {"GET", "/api/automation/rules"},
		{"POST", "/api/cells"}, {"POST", "/api/forms/abc/records"}, {"PUT", "/api/records/abc"},
		{"POST", "/api/automation/trigger/abc"}, {"POST", "/api/notifications/mark-read"},
		{"GET", "/api/business-admin/users"}, {"GET", "/api/admin/audit"}, {"GET", "/api/developer/workflows"},
		{"GET", "/api/forms//records"}, {"GET", "/api/dashboards/a/b"}, {"GET", "/api/integrations/abc/export"},
	} {
		if delegatedReadAllowed(c[0], c[1]) {
			t.Errorf("%s %s is open to a connector's read", c[0], c[1])
		}
	}

	f := setupConnector(t)
	h := NewHandlerWithDeps(logger.New("test"), f.pool, nil, Deps{})
	serve := func(ctx context.Context, method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(`{}`))
		req.Header.Set("X-App-Id", f.appID)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	gated := func(rec *httptest.ResponseRecorder) bool {
		return rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "not a read this connection may make")
	}
	// A read — a chat connector's, or a model link's source read — writes
	// nothing, the batch route included.
	read := withDelegatedSubject(context.Background(), f.admin)
	for _, c := range [][2]string{{"POST", "/api/cells"}, {"POST", "/api/cells/batch"}, {"GET", "/api/business-admin/users"}} {
		if rec := serve(read, c[0], c[1]); !gated(rec) {
			t.Errorf("%s %s as a connector's read: %d %s", c[0], c[1], rec.Code, rec.Body.String())
		}
	}
	// A chat connection's write reaches the batch route and nothing else.
	write := withDelegatedWrite(read, "claude-connector")
	for _, c := range [][2]string{{"POST", "/api/cells"}, {"POST", "/api/forms/abc/records"}, {"PUT", "/api/admin/connector-settings"}} {
		if rec := serve(write, c[0], c[1]); !gated(rec) {
			t.Errorf("%s %s as a connector's write: %d %s", c[0], c[1], rec.Code, rec.Body.String())
		}
	}
	if rec := serve(write, "POST", "/api/cells/batch"); gated(rec) {
		t.Errorf("the batch route refused a connector's write at the gate: %d %s", rec.Code, rec.Body.String())
	}
	// The write mark means nothing without a delegated subject.
	if _, ok := delegatedWrite(withDelegatedWrite(context.Background(), "x")); ok {
		t.Error("a write mark with no delegated subject counts as a delegated write")
	}
}

// callRaw calls tool and returns the whole result, content included.
func (c *mcpClient) callRaw(tool string, args map[string]any) *sdk.CallToolResult {
	c.t.Helper()
	res, err := c.cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		c.t.Fatalf("%s: protocol error: %v", tool, err)
	}
	return res
}

// Charts, comparisons and reports are made in the conversation from grid
// data, read as the person; nothing is saved in maverickbuilds.app.
func TestMCPConnectorChartsAndReports(t *testing.T) {
	f := setupConnector(t)
	f.setAccessRules(f.roID, member(f.geo["AMER"], "hidden"),
		map[string]string{"rule_type": "metric", "ref_id": f.metric["cost"], "access": "hidden"})
	reed := f.as(f.ro)
	revenue, target := f.metric["revenue"], f.metric["target"]
	ctx := context.Background()
	counts := func() string {
		var n string
		_ = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM model.dashboard_def) || '/' || (SELECT count(*) FROM model.dashboard_widget)
			|| '/' || (SELECT count(*) FROM model.grid_def)`).Scan(&n)
		return n
	}
	before := counts()
	byGeo := func(extra map[string]any) map[string]any {
		args := f.ctxArgs(map[string]any{"grid_id": f.gridID, "metric_ids": []string{revenue}, "group_by": f.geoDim, "leaves_only": true})
		for k, v := range extra {
			args[k] = v
		}
		return args
	}

	t.Run("a chart is the engine's values for the person, with a picture and a table", func(t *testing.T) {
		res := reed.callRaw("render_chart", byGeo(map[string]any{"kind": "bar", "title": "Revenue by region"}))
		if res.IsError {
			t.Fatalf("render_chart: %+v", res.Content)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		var view struct {
			View  string `json:"view"`
			Chart struct {
				Kind       string `json:"kind"`
				Categories []struct {
					ID string `json:"id"`
				} `json:"categories"`
				Series []struct {
					Values []*float64 `json:"values"`
				} `json:"series"`
			} `json:"chart"`
			Alternatives []string `json:"alternatives"`
		}
		_ = json.Unmarshal(raw, &view)
		if view.View != "chart" || view.Chart.Kind != "bar" {
			t.Fatalf("view: %s", raw)
		}
		for i, cat := range view.Chart.Categories {
			if cat.ID == "US" || cat.ID == "CA" || cat.ID == "AMER" {
				t.Errorf("the chart shows Reed %s", cat.ID)
			}
			want := f.restTotals(f.ro, map[string]string{f.geoDim: cat.ID})[revenue]
			if v := view.Chart.Series[0].Values[i]; v == nil || !nearly(*v, want) {
				t.Errorf("%s: chart %v, engine total %v", cat.ID, v, want)
			}
		}
		if len(view.Chart.Categories) != 2 {
			t.Errorf("categories = %v, want UK and DE", view.Chart.Categories)
		}
		if !slices.Contains(view.Alternatives, "pie") || slices.Contains(view.Alternatives, "scatter") {
			t.Errorf("alternatives = %v, want pie offered and scatter (one metric) not", view.Alternatives)
		}
		var text, png bool
		for _, c := range res.Content {
			switch x := c.(type) {
			case *sdk.TextContent:
				text = strings.Contains(x.Text, "| geography") && strings.Contains(x.Text, "United Kingdom")
			case *sdk.ImageContent:
				png = x.MIMEType == "image/png" && len(x.Data) > 1000 && string(x.Data[1:4]) == "PNG"
			}
		}
		if !text || !png {
			t.Errorf("content: text table %v, PNG %v", text, png)
		}
	})

	t.Run("a kind the data cannot honestly be drawn as is refused", func(t *testing.T) {
		for name, args := range map[string]map[string]any{
			"pie with parents beside children": byGeo(map[string]any{"kind": "pie", "leaves_only": false}),
			"line without a breakdown":         f.ctxArgs(map[string]any{"grid_id": f.gridID, "metric_ids": []string{revenue}, "kind": "line"}),
			"scatter of one metric":            byGeo(map[string]any{"kind": "scatter"}),
			"a hidden metric":                  byGeo(map[string]any{"metric_ids": []string{f.metric["cost"]}}),
		} {
			if res := reed.callRaw("render_chart", args); !res.IsError {
				t.Errorf("%s: drawn", name)
			}
		}
	})

	t.Run("a comparison is current minus baseline, member by member", func(t *testing.T) {
		cmp := reed.must("compare_grid", f.ctxArgs(map[string]any{
			"current":  map[string]any{"grid_id": f.gridID, "metric_ids": []string{revenue}, "group_by": f.geoDim, "leaves_only": true},
			"baseline": map[string]any{"grid_id": f.gridID, "metric_ids": []string{target}, "group_by": f.geoDim, "leaves_only": true},
		}))
		rows := cmp["rows"].([]any)
		if len(rows) != 2 {
			t.Fatalf("rows = %v, want UK and DE", rows)
		}
		for _, r := range rows {
			row := r.(map[string]any)
			code := row["code"].(string)
			tot := f.restTotals(f.ro, map[string]string{f.geoDim: code})
			want := tot[revenue] - tot[target]
			got, _ := row["difference"].(float64)
			if !nearly(got, want) {
				t.Errorf("%s: difference %v, want %v", code, got, want)
			}
			if pct, ok := row["percent_change"].(float64); ok && tot[target] != 0 && !nearly(pct, want/tot[target]*100) {
				t.Errorf("%s: percent %v", code, pct)
			}
		}
	})

	t.Run("a report reads every section and says when one could not be read", func(t *testing.T) {
		res := reed.callRaw("render_report", f.ctxArgs(map[string]any{"title": "Regional review", "sections": []map[string]any{
			{"heading": "Totals", "presentation": "kpi", "query": map[string]any{"grid_id": f.gridID, "metric_ids": []string{revenue, target}}},
			{"heading": "Revenue by region", "presentation": "bar", "commentary": "EMEA leads.",
				"query": map[string]any{"grid_id": f.gridID, "metric_ids": []string{revenue}, "group_by": f.geoDim, "leaves_only": true}},
			{"heading": "Against target", "presentation": "comparison",
				"query":    map[string]any{"grid_id": f.gridID, "metric_ids": []string{revenue}, "group_by": f.geoDim, "leaves_only": true},
				"baseline": map[string]any{"grid_id": f.gridID, "metric_ids": []string{target}, "group_by": f.geoDim, "leaves_only": true}},
			{"heading": "Costs", "presentation": "table", "query": map[string]any{"grid_id": f.gridID, "metric_ids": []string{f.metric["cost"]}}},
		}}))
		if res.IsError {
			t.Fatalf("render_report: %+v", res.Content)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		var view struct {
			Report struct {
				Complete bool `json:"complete"`
				Sections []struct {
					Status string `json:"status"`
				} `json:"sections"`
			} `json:"report"`
		}
		_ = json.Unmarshal(raw, &view)
		st := []string{}
		for _, s := range view.Report.Sections {
			st = append(st, s.Status)
		}
		if !slices.Equal(st, []string{"ok", "ok", "ok", "unavailable"}) || view.Report.Complete {
			t.Errorf("sections %v, complete %v — want the hidden-metric section unavailable and the report incomplete", st, view.Report.Complete)
		}
		if strings.Contains(string(raw), `"cost"`) {
			t.Error("the report names the hidden metric")
		}
	})

	t.Run("the in-chat view is served for the chart and report tools", func(t *testing.T) {
		list, _ := reed.cs.ListTools(ctx, nil)
		for _, tool := range list.Tools {
			ui, _ := tool.Meta["ui"].(map[string]any)
			wantUI := tool.Name == "render_chart" || tool.Name == "render_report"
			if (ui != nil && ui["resourceUri"] == "ui://maverickbuilds/view.html") != wantUI {
				t.Errorf("%s: _meta.ui = %v", tool.Name, ui)
			}
		}
		rr, err := reed.cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "ui://maverickbuilds/view.html"})
		if err != nil || len(rr.Contents) != 1 || rr.Contents[0].MIMEType != "text/html;profile=mcp-app" ||
			!strings.Contains(rr.Contents[0].Text, "ui/initialize") || strings.Contains(rr.Contents[0].Text, "innerHTML") {
			t.Errorf("view resource: %v", err)
		}
	})

	if after := counts(); after != before {
		t.Errorf("charting and reporting saved something: dashboards/widgets/grids %s before, %s after", before, after)
	}
}
