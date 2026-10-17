package aiassistant_test

import (
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// list_dashboards gave a text widget as "text at (0,0)": the AI developer
// could write a dashboard's instructions but never read them, so it could
// neither explain how an app is meant to be used nor edit its text without
// replacing what it had not seen. describe_dashboard reads one dashboard in
// full.
func TestDescribeDashboardReadsThePageInFull(t *testing.T) {
	h := newEditHarness(t)
	_, region := h.must("create_dimension", map[string]any{"name": "Region",
		"members": []map[string]any{{"code": "EU", "label": "Europe"}, {"code": "US", "label": "United States"}}})
	_, revenue := h.must("create_metric", map[string]any{"name": "Revenue", "is_input": true})
	_, grid := h.must("create_grid", map[string]any{"name": "Sales plan", "metric_ids": []string{revenue}, "dimension_ids": []string{region}})
	_, start := h.must("create_dashboard", map[string]any{"name": "Start"})
	_, inputs := h.must("create_dashboard", map[string]any{"name": "Inputs"})

	text := "# How to plan\nEnter revenue per region, then submit.\n" +
		"- [Enter the plan](dashboard:Inputs)\n" +
		"- [Same model, by its name](dashboard:Test Model/Inputs)\n" +
		"- [Old page](dashboard:Gone)\n" +
		"- [Sales](dashboard:Sales Model/Overview)\n" +
		"- [Formulas](/docs/formulas-manual/manual.html)"
	h.must("add_dashboard_widget", map[string]any{"dashboard_id": start, "widget_type": "text", "content": text,
		"pos_x": 0, "pos_y": 0, "size_w": 1200, "size_h": 200})
	h.must("add_dashboard_widget", map[string]any{"dashboard_id": start, "widget_type": "metric_kpi", "ref_id": revenue,
		"title": "EU revenue", "pos_x": 0, "pos_y": 240, "size_w": 300, "size_h": 120,
		"widget_props": map[string]any{"kpi_scope": map[string]any{"dimension_id": "Region", "member_code": "EU"}}})
	h.must("add_dashboard_widget", map[string]any{"dashboard_id": start, "widget_type": "chart", "ref_id": grid,
		"pos_x": 320, "pos_y": 240, "size_w": 600, "size_h": 380,
		"widget_props": map[string]any{"chart": map[string]any{"chart_type": "bar", "dimension_id": region, "metric_ids": []string{revenue}}}})
	// An image's content is the picture: it is never printed.
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO model.dashboard_widget (dashboard_id, widget_type, content, pos_x, pos_y)
		VALUES ($1::uuid, 'image', 'data:image/png;base64,SECRETPIXELS', 0, 700)`, start); err != nil {
		t.Fatal(err)
	}

	reader := aiassistant.NewToolExecutor(h.pool, h.modelID, h.revID)
	describe := func(ref string) string {
		t.Helper()
		out, err := reader.Execute(h.ctx, "describe_dashboard", mustJSON(t, map[string]any{"dashboard_id": ref}))
		if err != nil {
			t.Fatalf("describe_dashboard %q: %v", ref, err)
		}
		return out
	}
	out := describe("Start")
	for _, want := range []string{
		"Dashboard 'Start' (id:" + start + ")",
		"No business role is granted it yet",
		"   | # How to plan",
		"   | Enter revenue per region, then submit.",
		`"Enter the plan" → dashboard 'Inputs' (id:` + inputs + ")",
		`"Same model, by its name" → dashboard 'Inputs' (id:` + inputs + ")",
		`"Old page" → dashboard 'Gone', which the working revision does not have`,
		`"Sales" → dashboard 'Overview' of model 'Sales Model' (another model`,
		`"Formulas" → /docs/formulas-manual/manual.html`,
		"reads metric 'Revenue' (id:" + revenue + ")",
		`title "EU revenue"`,
		region + " = dimension Region",
		"reads grid 'Sales plan' (id:" + grid + ")",
		`"chart_type":"bar"`,
		revenue + " = metric Revenue",
		"4. image",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("describe_dashboard does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SECRETPIXELS") {
		t.Errorf("an image's picture was printed:\n%s", out)
	}
	// Reading order: the text above the KPI, the KPI left of the chart.
	if i, j, k := strings.Index(out, "1. text"), strings.Index(out, "2. metric_kpi"), strings.Index(out, "3. chart"); i < 0 || j < i || k < j {
		t.Errorf("widgets are not in reading order:\n%s", out)
	}
	if by := describe(start); by != out {
		t.Errorf("by id and by name differ:\n%s\n---\n%s", by, out)
	}

	_, role := h.must("create_business_role", map[string]any{"name": "Planners"})
	h.must("set_role_dashboards", map[string]any{"role": role, "dashboards": []string{"Start"}})
	if out := describe("start"); !strings.Contains(out, "Business roles that may open it: Planners") {
		t.Errorf("the role granted the dashboard is not named:\n%s", out)
	}
	if out := describe("Inputs"); !strings.Contains(out, "No widgets yet.") {
		t.Errorf("an empty dashboard does not say so:\n%s", out)
	}

	// An id from the revision the draft was copied from means the draft's
	// same-named copy; another model's dashboard is not read.
	revB := seedRevision(t, h.pool, h.modelID, "Rev B")
	_, startB, err := aiassistant.NewWriteExecutor(h.pool, h.modelID, revB).Execute(h.ctx, "create_dashboard", mustJSON(t, map[string]any{"name": "Start"}))
	if err != nil {
		t.Fatal(err)
	}
	outB, err := aiassistant.NewToolExecutor(h.pool, h.modelID, revB).Execute(h.ctx, "describe_dashboard", mustJSON(t, map[string]any{"dashboard_id": start}))
	if err != nil || !strings.Contains(outB, "(id:"+startB+")") {
		t.Errorf("rev A's id in rev B = %v\n%s; want rev B's Start (%s)", err, outB, startB)
	}
	other := seedModel(t, h.pool)
	otherRev := seedRevision(t, h.pool, other, "Other")
	_, foreign, err := aiassistant.NewWriteExecutor(h.pool, other, otherRev).Execute(h.ctx, "create_dashboard", mustJSON(t, map[string]any{"name": "Start"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{foreign, "Nowhere", ""} {
		if out, err := reader.Execute(h.ctx, "describe_dashboard", mustJSON(t, map[string]any{"dashboard_id": ref})); err == nil {
			t.Errorf("describe_dashboard %q: want a refusal, got\n%s", ref, out)
		}
	}
}
