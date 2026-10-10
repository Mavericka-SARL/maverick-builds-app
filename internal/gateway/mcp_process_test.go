package gateway

// The process around the grids through the chat connector: dashboards read
// as pages (text, links, what each widget shows — never its data) and
// published workflows with their steps, each as the person may see them.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestMCPConnectorReadsTheProcess(t *testing.T) {
	f := setupConnector(t)
	// Reed sees neither cost nor the US.
	f.setAccessRules(f.roID, member(f.geo["US"], "hidden"),
		map[string]string{"rule_type": "metric", "ref_id": f.metric["cost"], "access": "hidden"})

	// A developer builds two pages and a workflow, as in the console.
	devCall := func(method, path string, body any) map[string]any {
		t.Helper()
		status, raw := f.req(method, path, f.dev, body)
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	newDashboard := func(name string) string {
		return devCall("POST", "/api/developer/dashboards", map[string]any{"name": name, "revision_id": f.revID})["id"].(string)
	}
	overview, targets := newDashboard("Overview"), newDashboard("Targets")
	widget := func(dash string, body map[string]any) {
		devCall("POST", "/api/developer/dashboards/"+dash+"/widgets", body)
	}
	widget(overview, map[string]any{"widget_type": "text", "pos_y": 0,
		"content": "## How we plan\nEnter revenue first, then costs.\n\nNext: [Targets](dashboard:Targets) · [Guide](https://example.com/guide) · [Old page](dashboard:Retired page)"})
	widget(overview, map[string]any{"widget_type": "chart", "ref_id": f.gridID, "title": "Revenue by region", "pos_y": 1,
		"widget_props": map[string]any{"chart": map[string]any{"chart_type": "bar", "dimension_id": f.geoDim,
			"metric_ids": []string{f.metric["revenue"], f.metric["cost"]}, "context_defaults": map[string]string{f.periodDim: "Q1"}}}})
	widget(overview, map[string]any{"widget_type": "metric_kpi", "ref_id": f.metric["revenue"], "title": "US revenue", "pos_y": 2,
		"widget_props": map[string]any{"kpi_scope": map[string]string{"dimension_id": f.geoDim, "member_code": "US"}}})
	widget(overview, map[string]any{"widget_type": "image", "content": "data:image/png;base64,iVBORw0KGgo=", "pos_y": 3,
		"widget_props": map[string]any{"alt": "Planning calendar"}})

	wfPath := "/api/developer/workflows?application_id=" + f.appID + "&revision_id=" + f.revID
	wf := devCall("POST", wfPath, map[string]any{"name": "Budget approval", "trigger_event": "manual"})["id"].(string)
	devCall("PATCH", "/api/developer/workflows/"+wf, map[string]any{"description": "How a regional budget is approved", "steps": []map[string]any{
		{"id": "s1", "name": "Regional review", "type": "approval", "instructions": "Check revenue against target before approving.",
			"assignee_roles": []string{"business_admin"}, "sla_hours": 48, "routes": map[string]string{"approve": "end-completed", "reject": "end-rejected"},
			"condition":    map[string]any{"metric": f.metric["cost"], "op": ">", "value": 1000},
			"notification": map[string]any{"recipient_type": "role", "recipient_role": "business_admin"}},
	}})
	devCall("POST", "/api/developer/workflows/"+wf+"/publish", nil)

	reed, dev := f.as(f.ro), f.as(f.dev)

	t.Run("dashboards are listed as the person may open them", func(t *testing.T) {
		list := reed.must("list_dashboards", f.ctxArgs(nil))["dashboards"].([]any)
		var names []string
		for _, d := range list {
			names = append(names, d.(map[string]any)["name"].(string))
		}
		if !slices.Contains(names, "Overview") || !slices.Contains(names, "Targets") {
			t.Errorf("dashboards = %v", names)
		}
	})

	t.Run("a dashboard reads as a page: text, links, what each widget shows", func(t *testing.T) {
		page := reed.must("describe_dashboard", f.ctxArgs(map[string]any{"dashboard_id": overview}))
		raw, _ := json.Marshal(page)
		widgets := page["widgets"].([]any)
		if len(widgets) != 4 {
			t.Fatalf("widgets = %s", raw)
		}
		text := widgets[0].(map[string]any)
		if !strings.Contains(text["text"].(string), "Enter revenue first") {
			t.Errorf("text widget: %v", text)
		}
		links := text["links"].([]any)
		if len(links) != 3 {
			t.Fatalf("links = %v", links)
		}
		if l := links[0].(map[string]any); l["dashboard_id"] != targets || l["model_id"] != f.modelID {
			t.Errorf("the Targets link resolved to %v", l)
		}
		if l := links[1].(map[string]any); l["href"] != "https://example.com/guide" {
			t.Errorf("the web link: %v", l)
		}
		if l := links[2].(map[string]any); l["dashboard_id"] != nil || !strings.Contains(l["note"].(string), "not one you can open") {
			t.Errorf("a link to no dashboard: %v", l)
		}
		chart := widgets[1].(map[string]any)
		if chart["grid"].(map[string]any)["id"] != f.gridID || chart["chart_type"] != "bar" {
			t.Errorf("chart: %v", chart)
		}
		if ms := chart["metrics"].([]any); len(ms) != 1 || ms[0].(map[string]any)["id"] != f.metric["revenue"] {
			t.Errorf("chart metrics for Reed = %v, want revenue only (cost is hidden from him)", ms)
		}
		if fs := chart["filters"].([]any); len(fs) != 1 || fs[0].(map[string]any)["member_code"] != "Q1" {
			t.Errorf("chart filters = %v", fs)
		}
		kpi := widgets[2].(map[string]any)
		if kpi["type"] != "kpi" || kpi["filters"] != nil {
			t.Errorf("KPI pinned to the hidden US: %v — the member must not be named", kpi)
		}
		if img := widgets[3].(map[string]any); img["title"] != "Planning calendar" {
			t.Errorf("image: %v", img)
		}
		for _, leak := range []string{f.metric["cost"], `"US"`, "data:image", "iVBORw0KGgo"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("the page holds %s: %s", leak, raw)
			}
		}
	})

	t.Run("the developer sees what the widgets really name", func(t *testing.T) {
		page := dev.must("describe_dashboard", f.ctxArgs(map[string]any{"dashboard_id": overview}))
		widgets := page["widgets"].([]any)
		if ms := widgets[1].(map[string]any)["metrics"].([]any); len(ms) != 2 {
			t.Errorf("chart metrics for the developer = %v", ms)
		}
		if fs := widgets[2].(map[string]any)["filters"].([]any); len(fs) != 1 || fs[0].(map[string]any)["member_code"] != "US" {
			t.Errorf("KPI filter for the developer = %v", fs)
		}
	})

	t.Run("a dashboard the person is not listed is not read", func(t *testing.T) {
		if _, errText := reed.call("describe_dashboard", f.ctxArgs(map[string]any{"dashboard_id": "00000000-0000-0000-0000-000000000001"})); !strings.HasPrefix(errText, "not_found") {
			t.Errorf("an unknown dashboard: %q", errText)
		}
	})

	t.Run("a workflow reads with its steps, never its conditions or recipients", func(t *testing.T) {
		list := reed.must("list_workflows", f.ctxArgs(nil))["workflows"].([]any)
		if len(list) != 1 || list[0].(map[string]any)["id"] != wf || list[0].(map[string]any)["description"] != "How a regional budget is approved" {
			t.Fatalf("workflows = %v", list)
		}
		desc := reed.must("describe_workflow", f.ctxArgs(map[string]any{"workflow_id": wf}))
		raw, _ := json.Marshal(desc)
		steps := desc["steps"].([]any)
		s := steps[0].(map[string]any)
		if s["name"] != "Regional review" || s["type"] != "approval" || s["instructions"] != "Check revenue against target before approving." ||
			s["sla_hours"] != float64(48) || s["assignee_roles"].([]any)[0] != "business_admin" {
			t.Errorf("step = %v", s)
		}
		for _, leak := range []string{"condition", "recipient", f.metric["cost"]} {
			if strings.Contains(string(raw), `"`+leak) || strings.Contains(string(raw), leak+`"`) {
				t.Errorf("the workflow holds %s: %s", leak, raw)
			}
		}
	})
}
