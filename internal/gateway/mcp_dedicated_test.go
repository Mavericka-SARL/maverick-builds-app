package gateway

// The chat connector where the hosted service runs it: every tenant in a
// database of its own (TENANT_DB_MODE=dedicated). A connector's read is
// routed like a console request — by the application it names and the
// person's own homes — so a person reads their tenant's grids and nothing of
// another tenant's.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/salesdemo"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

func TestMCPConnectorInDedicatedTenants(t *testing.T) {
	f := newDedicatedFixture(t, Deps{MCP: MCPConfig{Enabled: true, ResourceURL: "http://connector.test/mcp"}})

	// Acme: a developer builds the sales model through the API; a business
	// user reads it.
	_, wsA, tctxA := f.tenant(t, "Acme")
	appA, modelA := f.appIn(t, tctxA, wsA, "Acme Sales")
	var revA string
	if err := f.reader.db.QueryRow(tctxA, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelA).Scan(&revA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reader.db.Exec(tctxA, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revA, modelA); err != nil {
		t.Fatal(err)
	}
	f.seed(t, tctxA, "acme-dev", "dev@acme.test", "Dana", "developer", wsA)
	f.seed(t, tctxA, "acme-bu", "bu@acme.test", "Bo", "business_user", wsA)
	f.seed(t, tctxA, "acme-ta", "ta@acme.test", "Tia", "tenant_admin", wsA)
	caller := func(method, path string, body any) (map[string]any, error) {
		code, raw := do(t, f.srv, call{persona: "acme-dev", app: appA}, method, path, body)
		if code < 200 || code >= 300 {
			return nil, fmt.Errorf("%s %s: %d %s", method, path, code, raw)
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out, nil
	}
	m, err := salesdemo.Build(caller, revA)
	if err != nil {
		t.Fatalf("build the model in Acme's database: %v", err)
	}
	if err := m.WriteFacts(caller, modelA, revA, salesdemo.SampleFacts()); err != nil {
		t.Fatalf("facts: %v", err)
	}

	// Beta: another tenant, another database, its own business user.
	_, wsB, tctxB := f.tenant(t, "Beta")
	appB, _ := f.appIn(t, tctxB, wsB, "Beta Ops")
	f.seed(t, tctxB, "beta-bu", "bu@beta.test", "Bea", "business_user", wsB)

	acme := connectMCP(t, f.srv.URL, "dev:acme-bu")
	beta := connectMCP(t, f.srv.URL, "dev:beta-bu")
	args := func(extra map[string]any) map[string]any {
		a := map[string]any{"application_id": appA, "model_id": modelA}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}

	t.Run("a person reads their own tenant's grids, as the console does", func(t *testing.T) {
		models := acme.must("list_models", nil)["models"].([]any)
		if len(models) != 1 || models[0].(map[string]any)["model_id"] != modelA {
			t.Fatalf("Acme's business user lists %v", models)
		}
		revenue := m.Metric["revenue"]
		res := acme.must("query_grid", args(map[string]any{"grid_id": m.GridID, "metric_ids": []string{revenue}}))
		got, state := valueOf(t, res["rows"].([]any)[0].(map[string]any)["value"])
		q := url.Values{"grid_def_id": {m.GridID}, "totals_only": {"1"}}
		code, raw := do(t, f.srv, call{persona: "acme-bu", app: appA}, http.MethodGet, "/api/grid?"+q.Encode(), nil)
		var g struct {
			Totals map[string]float64 `json:"totals"`
		}
		if code != http.StatusOK || json.Unmarshal(raw, &g) != nil {
			t.Fatalf("REST totals: %d %s", code, raw)
		}
		if state != "ok" || !nearly(got, g.Totals[revenue]) || got == 0 {
			t.Errorf("connector total %v (%s), REST %v", got, state, g.Totals[revenue])
		}
		chart := acme.callRaw("render_chart", args(map[string]any{"grid_id": m.GridID, "metric_ids": []string{revenue}, "group_by": m.GeoDim, "leaves_only": true}))
		if chart.IsError {
			t.Errorf("render_chart in a dedicated tenant: %+v", chart.Content)
		}
	})

	t.Run("another tenant's person reads none of it", func(t *testing.T) {
		models := beta.must("list_models", nil)["models"].([]any)
		for _, mm := range models {
			if mm.(map[string]any)["model_id"] == modelA {
				t.Errorf("Beta's business user is listed Acme's model")
			}
		}
		if _, errText := beta.call("list_sources", args(nil)); !strings.HasPrefix(errText, "not_found") {
			t.Errorf("Beta reading Acme's model: %q", errText)
		}
		if _, errText := beta.call("list_sources", map[string]any{"application_id": appB, "model_id": modelA}); !strings.HasPrefix(errText, "not_found") {
			t.Errorf("Acme's model under Beta's application: %q", errText)
		}
		if _, errText := beta.call("query_grid", args(map[string]any{"grid_id": m.GridID})); !strings.HasPrefix(errText, "not_found") {
			t.Errorf("Beta querying Acme's grid: %q", errText)
		}
	})

	units := func(v float64) map[string]any {
		return args(map[string]any{"cells": []any{map[string]any{"metric_id": m.Metric["units"],
			"members": map[string]string{m.GeoDim: "UK", m.ProdDim: "LAPTOP", m.PeriodDim: "Q1"}, "value": v}}})
	}
	ukUnits := func() float64 {
		t.Helper()
		var v float64
		_ = f.reader.db.QueryRow(tctxA, `SELECT value FROM runtime.fact_input WHERE metric_id=$1::uuid AND dim_members->>$2 = 'UK'
			AND dim_members->>$3 = 'LAPTOP' AND dim_members->>$4 = 'Q1' ORDER BY entered_at DESC LIMIT 1`,
			m.Metric["units"], m.GeoDim, m.ProdDim, m.PeriodDim).Scan(&v)
		return v
	}

	t.Run("a person writes their own tenant's cells, in its database", func(t *testing.T) {
		if res := acme.must("write_cells", units(950)); res["status"] != "written" {
			t.Fatalf("write_cells: %v", res)
		}
		if v := ukUnits(); !nearly(v, 950) {
			t.Errorf("units in Acme's database = %v, want 950", v)
		}
		if _, errText := beta.call("write_cells", units(1)); !strings.HasPrefix(errText, "not_found") {
			t.Errorf("Beta writing Acme's cells: %q", errText)
		}
		if v := ukUnits(); !nearly(v, 950) {
			t.Errorf("units after Beta's attempt = %v, want 950", v)
		}
	})

	t.Run("the tenant's administrator turns chat writes off for that tenant", func(t *testing.T) {
		code, raw := do(t, f.srv, call{persona: "acme-ta", app: appA}, http.MethodPut, "/api/admin/connector-settings", map[string]any{"chat_writes": false})
		if code != http.StatusOK {
			t.Fatalf("switch off: %d %s", code, raw)
		}
		if _, errText := acme.call("write_cells", units(1)); !strings.Contains(errText, "turned off changing data from chat") {
			t.Errorf("a chat write in a tenant that turned them off: %q", errText)
		}
		if v := ukUnits(); !nearly(v, 950) {
			t.Errorf("units = %v, want 950", v)
		}
		code, raw = do(t, f.srv, call{persona: "acme-ta", app: appA}, http.MethodGet, "/api/admin/connector-settings", nil)
		if code != http.StatusOK || !strings.Contains(string(raw), `"chat_writes":false`) {
			t.Errorf("Acme's settings: %d %s", code, raw)
		}
	})
}

func TestConnectorInfoForEveryone(t *testing.T) {
	d := setupSalesDemo(t)
	get := func(srv *httptest.Server, persona string) (int, map[string]any) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/connector", nil)
		req.Header.Set("X-Dev-User", persona)
		req.Header.Set("X-App-Id", d.appID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	on := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), d.pool, nil, Deps{MCP: MCPConfig{
		Enabled: true, ResourceURL: "https://app.example.test/mcp",
		ClientSecrets: map[string]string{"claude-connector": "claude-secret", "chatgpt-connector": "chatgpt-secret"},
	}}))
	defer on.Close()

	status, info := get(on, d.dev)
	if status != http.StatusOK || info["enabled"] != true || info["url"] != "https://app.example.test/mcp" {
		t.Fatalf("developer: %d %v", status, info)
	}
	raw, _ := json.Marshal(info["hosts"])
	for _, want := range []string{`"client_id":"chatgpt-connector"`, `"client_secret":"chatgpt-secret"`, `"name":"Claude"`, `"client_secret":"claude-secret"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("hosts %s lack %s", raw, want)
		}
	}
	if status, info := get(on, d.westRep); status != http.StatusOK || info["enabled"] != true {
		t.Errorf("business user: %d %v — every signed-in person finds the details in the account menu", status, info)
	}
	if status, info := get(d.srv, d.dev); status != http.StatusOK || info["enabled"] != false {
		t.Errorf("connector off: %d %v", status, info)
	}
}
