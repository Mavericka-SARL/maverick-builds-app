package gateway

// The ChatGPT/Claude plugin submission's test cases, run end to end on what
// a reviewer's account holds: a fresh self-service sign-up (the Basic
// workspace), whose starter models are the same for everyone. The expected
// results written in docs/CHAT_PLUGIN_SUBMISSION.md are the numbers asserted
// here — change one, change both.

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// asList is v as a JSON list; absent is empty.
func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func TestChatPluginReviewCases(t *testing.T) {
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("DEV_MODE", "true")
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{
		Signup: SignupConfig{Enabled: true},
		MCP:    MCPConfig{Enabled: true, ResourceURL: "https://app.example.test/mcp", OpenAIAppsChallenge: "challenge-token-123"},
	}))
	t.Cleanup(srv.Close)

	code, out := callJSON(t, srv, "", http.MethodPost, "/api/signup", map[string]any{
		"company": "Plugin Review", "first_name": "Rae", "last_name": "Viewer", "email": "review@example.test"})
	if code != http.StatusOK || out["dev_persona"] == nil {
		t.Fatalf("sign-up: %d %v", code, out)
	}
	c := connectMCP(t, srv.URL, "dev:"+out["dev_persona"].(string))

	t.Run("the domain-verification challenge is served", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/.well-known/openai-apps-challenge") //nolint:noctx
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "challenge-token-123" {
			t.Errorf("challenge: %d %q", resp.StatusCode, body)
		}
	})

	// Case 1 — "Which models can I access?"
	models := map[string]map[string]any{}
	for _, m := range c.must("list_models", nil)["models"].([]any) {
		models[m.(map[string]any)["model_name"].(string)] = m.(map[string]any)
	}
	for _, name := range []string{"Learn the platform", "Developer guide", "Business admin guide", "Tenant admin guide"} {
		if models[name] == nil {
			t.Fatalf("case 1: %q not listed (have %v)", name, models)
		}
	}
	ctxOf := func(name string, extra map[string]any) map[string]any {
		a := map[string]any{"application_id": models[name]["application_id"], "model_id": models[name]["model_id"]}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	// ids by name, from discovery, as the assistant finds them.
	grid := func(model, gridName string) (id string, metrics, dims map[string]string) {
		for _, g := range c.must("list_sources", ctxOf(model, nil))["grids"].([]any) {
			if g.(map[string]any)["name"] == gridName {
				id = g.(map[string]any)["id"].(string)
			}
		}
		if id == "" {
			t.Fatalf("grid %q not in %q", gridName, model)
		}
		d := c.must("describe_source", ctxOf(model, map[string]any{"source_id": id}))
		metrics, dims = map[string]string{}, map[string]string{}
		for _, m := range d["metrics"].([]any) {
			metrics[m.(map[string]any)["name"].(string)] = m.(map[string]any)["id"].(string)
		}
		for _, x := range d["dimensions"].([]any) {
			dims[x.(map[string]any)["name"].(string)] = x.(map[string]any)["id"].(string)
		}
		return id, metrics, dims
	}
	tourGrid, tm, td := grid("Learn the platform", "Team cost by quarter")
	byMember := func(rows []any, metricID string) map[string]float64 {
		out := map[string]float64{}
		for _, r := range rows {
			row := r.(map[string]any)
			v, state := valueOf(t, row[metricID])
			if state != "ok" {
				t.Errorf("%v: state %s", row["member_code"], state)
			}
			out[row["member_code"].(string)] = v
		}
		return out
	}
	want := func(label string, got, exp map[string]float64) {
		t.Helper()
		if len(got) != len(exp) {
			t.Errorf("%s: %v, want %v", label, got, exp)
		}
		for k, v := range exp {
			if !nearly(got[k], v) {
				t.Errorf("%s: %s = %v, want %v", label, k, got[k], v)
			}
		}
	}

	t.Run("case 2 — total cost per team this year", func(t *testing.T) {
		res := c.must("query_grid", ctxOf("Learn the platform", map[string]any{
			"grid_id": tourGrid, "metric_ids": []string{tm["cost"]}, "group_by": td["team"], "leaves_only": true}))
		want("cost by team", byMember(res["rows"].([]any), tm["cost"]), map[string]float64{"SALES": 216000, "ENG": 405000})
	})

	t.Run("case 3 — year-end headcount is the last quarter's, not a sum", func(t *testing.T) {
		res := c.must("query_grid", ctxOf("Learn the platform", map[string]any{
			"grid_id": tourGrid, "metric_ids": []string{tm["headcount"]}, "group_by": td["team"], "leaves_only": true}))
		want("headcount by team", byMember(res["rows"].([]any), tm["headcount"]), map[string]float64{"SALES": 5, "ENG": 8})
		total := c.must("query_grid", ctxOf("Learn the platform", map[string]any{"grid_id": tourGrid, "metric_ids": []string{tm["headcount"]}}))
		if v, _ := valueOf(t, total["rows"].([]any)[0].(map[string]any)["value"]); !nearly(v, 13) {
			t.Errorf("year-end headcount = %v, want 13 (5 + 8, the last quarter's)", v)
		}
	})

	t.Run("case 4 — cost by quarter as a line chart", func(t *testing.T) {
		res := c.callRaw("render_chart", ctxOf("Learn the platform", map[string]any{
			"grid_id": tourGrid, "metric_ids": []string{tm["cost"]}, "group_by": td["quarter"], "leaves_only": true, "kind": "line"}))
		if res.IsError {
			t.Fatalf("render_chart: %+v", res.Content)
		}
		view := res.StructuredContent.(map[string]any)
		want("cost by quarter", byMember(view["data"].(map[string]any)["rows"].([]any), tm["cost"]),
			map[string]float64{"Q1": 138000, "Q2": 138000, "Q3": 165000, "Q4": 180000})
	})

	t.Run("case 5 — spent against budget by office, and a report", func(t *testing.T) {
		baGrid, bm, bd := grid("Business admin guide", "Budget by office")
		cmp := c.must("compare_grid", ctxOf("Business admin guide", map[string]any{
			"current":  map[string]any{"grid_id": baGrid, "metric_ids": []string{bm["spent"]}, "group_by": bd["office"], "leaves_only": true},
			"baseline": map[string]any{"grid_id": baGrid, "metric_ids": []string{bm["budget"]}, "group_by": bd["office"], "leaves_only": true},
		}))
		expDiff := map[string]float64{"LISBON": -3500, "MADRID": -38000, "OSLO": -36000, "STOCKHOLM": -9000}
		expPct := map[string]float64{"LISBON": -5, "MADRID": -42.2222, "OSLO": -30, "STOCKHOLM": -9}
		for _, r := range cmp["rows"].([]any) {
			row := r.(map[string]any)
			code := row["code"].(string)
			if d, _ := row["difference"].(float64); !nearly(d, expDiff[code]) {
				t.Errorf("%s difference %v, want %v", code, d, expDiff[code])
			}
			if p, _ := row["percent_change"].(float64); math.Abs(p-expPct[code]) > 0.01 {
				t.Errorf("%s percent %v, want %v", code, p, expPct[code])
			}
		}
		rep := c.callRaw("render_report", ctxOf("Business admin guide", map[string]any{"title": "Office spending", "sections": []map[string]any{
			{"heading": "Totals", "presentation": "kpi", "query": map[string]any{"grid_id": baGrid, "metric_ids": []string{bm["budget"], bm["spent"], bm["remaining"]}}},
			{"heading": "Spent against budget", "presentation": "comparison",
				"query":    map[string]any{"grid_id": baGrid, "metric_ids": []string{bm["spent"]}, "group_by": bd["office"], "leaves_only": true},
				"baseline": map[string]any{"grid_id": baGrid, "metric_ids": []string{bm["budget"]}, "group_by": bd["office"], "leaves_only": true}},
		}}))
		if rep.IsError {
			t.Fatalf("render_report: %+v", rep.Content)
		}
		kpis := rep.StructuredContent.(map[string]any)["report"].(map[string]any)["sections"].([]any)[0].(map[string]any)["data"].(map[string]any)["rows"].([]any)
		got := map[string]float64{}
		for _, r := range kpis {
			v, _ := valueOf(t, r.(map[string]any)["value"])
			got[r.(map[string]any)["metric_id"].(string)] = v
		}
		want("office totals", got, map[string]float64{bm["budget"]: 380000, bm["spent"]: 293500, bm["remaining"]: 86500})
	})

	t.Run("case 8 — walk me through the Business admin guide", func(t *testing.T) {
		var first string
		for _, d := range c.must("list_dashboards", ctxOf("Business admin guide", nil))["dashboards"].([]any) {
			if d.(map[string]any)["name"] == "1 · Your part" {
				first = d.(map[string]any)["id"].(string)
			}
		}
		if first == "" {
			t.Fatal("the guide's first page is not listed")
		}
		page := c.must("describe_dashboard", ctxOf("Business admin guide", map[string]any{"dashboard_id": first}))
		var next map[string]any
		for _, w := range page["widgets"].([]any) {
			wm := w.(map[string]any)
			if wm["type"] != "text" {
				continue
			}
			for _, l := range asList(wm["links"]) {
				if l.(map[string]any)["dashboard_name"] == "2 · The model you work in" {
					next = l.(map[string]any)
				}
			}
		}
		if next == nil || next["dashboard_id"] == nil {
			t.Fatalf("page 1 does not lead to page 2: %v", page["widgets"])
		}
		page2 := c.must("describe_dashboard", map[string]any{"application_id": next["application_id"], "model_id": next["model_id"], "dashboard_id": next["dashboard_id"]})
		if page2["dashboard"].(map[string]any)["name"] != "2 · The model you work in" {
			t.Errorf("following the link opened %v", page2["dashboard"])
		}
	})

	// Cases 6 and 7 change the account, so they run last, and 7 puts back
	// what 6 changed: the next reviewer reads the numbers above.
	headcount := func(team, quarter string) map[string]any {
		return map[string]any{"metric_id": tm["headcount"], "members": map[string]string{td["team"]: team, td["quarter"]: quarter}}
	}
	yearEnd := func() float64 {
		total := c.must("query_grid", ctxOf("Learn the platform", map[string]any{"grid_id": tourGrid, "metric_ids": []string{tm["headcount"]}}))
		v, _ := valueOf(t, total["rows"].([]any)[0].(map[string]any)["value"])
		return v
	}

	t.Run("case 6 — set Engineering's headcount in Q4 to 10", func(t *testing.T) {
		cell := headcount("ENG", "Q4")
		cell["value"] = 10
		if res := c.must("write_cells", ctxOf("Learn the platform", map[string]any{"cells": []any{cell}})); res["status"] != "written" {
			t.Fatalf("write_cells: %v", res)
		}
		if v := yearEnd(); !nearly(v, 15) {
			t.Errorf("year-end headcount = %v, want 15 (5 + 10)", v)
		}
		res := c.must("query_grid", ctxOf("Learn the platform", map[string]any{
			"grid_id": tourGrid, "metric_ids": []string{tm["cost"]}, "group_by": td["team"], "leaves_only": true}))
		want("cost by team", byMember(res["rows"].([]any), tm["cost"]), map[string]float64{"SALES": 216000, "ENG": 435000})
	})

	t.Run("case 7 — set it back to 8", func(t *testing.T) {
		cell := headcount("ENG", "Q4")
		cell["value"] = 8
		c.must("write_cells", ctxOf("Learn the platform", map[string]any{"cells": []any{cell}}))
		if v := yearEnd(); !nearly(v, 13) {
			t.Errorf("year-end headcount = %v, want 13 again", v)
		}
	})

	t.Run("negative cases — only input cells can be changed", func(t *testing.T) {
		list, err := c.cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range list.Tools {
			if (tool.Name != "write_cells" && !tool.Annotations.ReadOnlyHint) || strings.HasPrefix(tool.Name, "update") || strings.HasPrefix(tool.Name, "delete") {
				t.Errorf("%s is not a pure read", tool.Name)
			}
		}
		// Negative case 1: cost is calculated, so a write to it is refused
		// and nothing is written.
		cell := map[string]any{"metric_id": tm["cost"], "members": map[string]string{td["team"]: "ENG", td["quarter"]: "Q4"}, "value": 100000}
		if _, errText := c.call("write_cells", ctxOf("Learn the platform", map[string]any{"cells": []any{cell}})); !strings.Contains(errText, "metric is not writable") ||
			!strings.HasPrefix(errText, "refused: nothing was written") {
			t.Errorf("a write to the calculated cost: %q", errText)
		}
		res := c.must("query_grid", ctxOf("Learn the platform", map[string]any{
			"grid_id": tourGrid, "metric_ids": []string{tm["cost"]}, "group_by": td["team"], "leaves_only": true}))
		want("cost by team", byMember(res["rows"].([]any), tm["cost"]), map[string]float64{"SALES": 216000, "ENG": 405000})
	})
}
