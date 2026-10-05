package aiassistant_test

// The AI Developer's change-and-remove tools (2026-09-28), and the defects the
// parity audit that added them found. Each tool is held to what its
// developer endpoint does; the endpoint is named in the tool's comment.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

type editHarness struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	modelID string
	revID   string
	exec    *aiassistant.WriteExecutor
}

func newEditHarness(t *testing.T) *editHarness {
	t.Helper()
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	h := &editHarness{t: t, ctx: context.Background(), pool: pool, modelID: modelID, revID: revID}
	h.exec = aiassistant.NewWriteExecutor(pool, modelID, revID)
	return h
}

func (h *editHarness) run(tool string, params map[string]any) (string, string, error) {
	h.t.Helper()
	return h.exec.Execute(h.ctx, tool, mustJSON(h.t, params))
}

func (h *editHarness) must(tool string, params map[string]any) (string, string) {
	h.t.Helper()
	res, id, err := h.run(tool, params)
	if err != nil {
		h.t.Fatalf("%s: %v", tool, err)
	}
	return res, id
}

func (h *editHarness) refused(what string, err error, want string) {
	h.t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		h.t.Errorf("%s: err %v, want it refused with %q", what, err, want)
	}
}

func (h *editHarness) scalar(sql string, args ...any) string {
	h.t.Helper()
	var s string
	if err := h.pool.QueryRow(h.ctx, sql, args...).Scan(&s); err != nil {
		h.t.Fatalf("query %q: %v", sql, err)
	}
	return s
}

func (h *editHarness) user() string {
	return h.scalar(`INSERT INTO identity.user (keycloak_sub, email) VALUES ('dev-sub', 'dev@example.com') RETURNING id::text`)
}

func (h *editHarness) fact(metricID string, members map[string]string, value float64) {
	h.t.Helper()
	dm, _ := json.Marshal(members)
	if _, err := h.pool.Exec(h.ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, 'Rev A', $3::uuid, $4::jsonb, $5)`,
		h.modelID, h.revID, metricID, string(dm), value); err != nil {
		h.t.Fatalf("seed fact: %v", err)
	}
}

// ── defects the audit found ───────────────────────────────────────────────────

// create_grid attached metric_ids and dimension_ids with raw inserts: another
// model's metric and a metric of another revision went under the new grid,
// a metric named by name was refused, and a dimension was reported attached
// when it was not.
func TestCreateGrid_AttachesThroughTheGridChecks(t *testing.T) {
	h := newEditHarness(t)
	revB := seedRevision(t, h.pool, h.modelID, "Rev B")
	otherModel := seedModel(t, h.pool)
	otherRev := seedRevision(t, h.pool, otherModel, "Other")
	revBOnly := h.scalar(`INSERT INTO model.metric_def (model_id, name, is_input, revision_id, agg_rule) VALUES ($1::uuid,'rev_b_only',true,$2::uuid,'sum') RETURNING id::text`, h.modelID, revB)
	foreign := h.scalar(`INSERT INTO model.metric_def (model_id, name, is_input, revision_id, agg_rule) VALUES ($1::uuid,'foreign',true,$2::uuid,'sum') RETURNING id::text`, otherModel, otherRev)
	h.must("create_metric", map[string]any{"name": "revenue", "is_input": true})
	h.must("create_dimension", map[string]any{"name": "region"})

	// Another model's metric and another revision's are refused, and the
	// step fails as a whole — a grid missing what was proposed is not built.
	_, _, err := h.run("create_grid", map[string]any{
		"name": "G", "metric_ids": []string{revBOnly, foreign, "revenue"}, "dimension_ids": []string{"region"},
	})
	for _, want := range []string{"could not attach", "different model", "no counterpart"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("create_grid err = %v, want it to name %q", err, want)
		}
	}
	if n := h.scalar(`SELECT count(*)::text FROM model.grid_def WHERE model_id=$1::uuid AND name='G'`, h.modelID); n != "0" {
		t.Errorf("a failed create_grid left %s grid(s) behind", n)
	}
	if n := h.scalar(`SELECT count(*)::text FROM model.grid_metric WHERE metric_id=$1::uuid`, foreign); n != "0" {
		t.Errorf("another model's metric was put on this model's grid")
	}

	// By name, and with what this revision has, it attaches.
	msg, gridID := h.must("create_grid", map[string]any{"name": "G", "metrics": []string{"revenue"}, "dimensions": []string{"region"}})
	if !strings.Contains(msg, "1 metrics, 1 dimensions attached") {
		t.Errorf("result %q", msg)
	}
	if n := h.scalar(`SELECT count(*)::text FROM model.grid_dimension WHERE grid_id=$1::uuid`, gridID); n != "1" {
		t.Errorf("attached dimensions = %s, want region", n)
	}
}

// generate_migration always failed (it read a table that does not exist) and
// apply_migration did nothing while reporting success; migrations run on
// promote. Neither is a tool any more, and the schema, the prompt and the
// executor agree on what is.
func TestWriteToolNames_MatchExecutorSchemaAndPrompt(t *testing.T) {
	h := newEditHarness(t)
	for _, gone := range []string{"generate_migration", "apply_migration"} {
		if _, _, err := h.run(gone, map[string]any{}); err == nil || !strings.Contains(err.Error(), "unknown write tool") {
			t.Errorf("%s: err %v, want unknown write tool", gone, err)
		}
	}
	prompt := aiassistant.BuildSystemPrompt(aiassistant.ModelContext{})
	var schema string
	for _, tool := range aiassistant.AllTools() {
		if tool.Name == "propose_actions" {
			schema = string(tool.Parameters)
		}
	}
	for _, name := range aiassistant.WriteToolNames {
		if _, _, err := h.run(name, map[string]any{}); err != nil && strings.Contains(err.Error(), "unknown write tool") {
			t.Errorf("%s is offered but the executor does not know it", name)
		}
		if !strings.Contains(prompt, name) {
			t.Errorf("%s is not in the prompt", name)
		}
		if !strings.Contains(schema, `"`+name+`"`) {
			t.Errorf("%s is not in the propose_actions enum", name)
		}
	}
	if strings.Contains(prompt, "generate_migration") || strings.Contains(schema, "apply_migration") {
		t.Error("a removed migration tool is still offered")
	}
}

// The prompt's model summary counted dimensions, grids and dashboards across
// every revision.
func TestFetchModelContext_CountsTheWorkingRevision(t *testing.T) {
	h := newEditHarness(t)
	revB := seedRevision(t, h.pool, h.modelID, "Rev B")
	for _, rev := range []string{h.revID, revB} {
		for _, n := range []string{"region", "product"} {
			h.scalar(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,$3) RETURNING id::text`, h.modelID, rev, n)
		}
		h.scalar(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'G') RETURNING id::text`, h.modelID, rev)
	}
	mc := aiassistant.FetchModelContext(h.ctx, h.pool, h.modelID, h.revID)
	if mc.DimCount != 2 || mc.GridCount != 1 || mc.WorkingRev != "Rev A" {
		t.Errorf("context = %+v, want 2 dimensions and 1 grid of Rev A", mc)
	}
}

// The developer's metric and member endpoints refuse what the tenant's plan
// does not allow; the assistant ran no plan check at all.
func TestPlanLimitHooks(t *testing.T) {
	h := newEditHarness(t)
	limit := errors.New("plan limit reached")
	var metricAsks, memberAsks []int
	h.exec.WithHooks(aiassistant.Hooks{
		CheckMetrics: func(_ context.Context, _ string, n int) error { metricAsks = append(metricAsks, n); return limit },
		CheckMembers: func(_ context.Context, _ string, n int) error { memberAsks = append(memberAsks, n); return limit },
	})
	_, _, err := h.run("create_metric", map[string]any{"name": "revenue", "is_input": true})
	h.refused("create_metric over the limit", err, "plan limit")
	_, _, err = h.run("create_dimension", map[string]any{"name": "region", "members": []map[string]any{{"code": "A", "label": "A"}, {"code": "B", "label": "B"}}})
	h.refused("create_dimension with members over the limit", err, "plan limit")
	h.exec.WithHooks(aiassistant.Hooks{})
	h.must("create_dimension", map[string]any{"name": "region"})
	h.must("create_dimension", map[string]any{"name": "month", "dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1})
	h.exec.WithHooks(aiassistant.Hooks{CheckMembers: func(_ context.Context, _ string, n int) error { memberAsks = append(memberAsks, n); return limit }})
	_, _, err = h.run("add_dimension_member", map[string]any{"dimension_id": "region", "code": "EMEA", "label": "EMEA"})
	h.refused("add_dimension_member over the limit", err, "plan limit")
	_, _, err = h.run("generate_time_members", map[string]any{"dimension_id": "month", "start": "2026-01-01", "end": "2026-12-31"})
	h.refused("generate_time_members over the limit", err, "plan limit")
	// 1 metric; then 2 members at create_dimension, 1 added member, 12 generated months.
	if fmt.Sprint(metricAsks) != "[1]" || fmt.Sprint(memberAsks) != "[2 1 12]" {
		t.Errorf("plan asks: metrics %v, members %v", metricAsks, memberAsks)
	}
	if n := h.scalar(`SELECT count(*)::text FROM model.dimension_member dm JOIN model.dimension_def d ON d.id=dm.dimension_id WHERE d.model_id=$1::uuid`, h.modelID); n != "0" {
		t.Errorf("%s member(s) written past the limit", n)
	}
}

// delete_metric now also takes the metric out of every chart's series, as
// the developer's delete does; a chart left plotting nothing goes.
func TestDeleteMetric_LeavesChartSeries(t *testing.T) {
	h := newEditHarness(t)
	_, rev := h.must("create_metric", map[string]any{"name": "revenue", "is_input": true})
	_, cost := h.must("create_metric", map[string]any{"name": "cost", "is_input": true})
	_, grid := h.must("create_grid", map[string]any{"name": "G", "metric_ids": []string{rev, cost}})
	_, dash := h.must("create_dashboard", map[string]any{"name": "D"})
	_, both := h.must("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "chart", "ref_id": grid,
		"widget_props": map[string]any{"chart": map[string]any{"chart_type": "bar", "metric_ids": []string{rev, cost}}}})
	_, only := h.must("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "chart", "ref_id": grid, "pos_y": 400,
		"widget_props": map[string]any{"chart": map[string]any{"chart_type": "bar", "metric_ids": []string{rev}}}})
	h.must("delete_metric", map[string]any{"metric_id": rev})
	if s := h.scalar(`SELECT widget_props->'chart'->>'metric_ids' FROM model.dashboard_widget WHERE id=$1::uuid`, both); strings.Contains(s, rev) {
		t.Errorf("deleted metric still plotted: %s", s)
	}
	if n := h.scalar(`SELECT count(*)::text FROM model.dashboard_widget WHERE id=$1::uuid`, only); n != "0" {
		t.Error("a chart left plotting nothing was kept")
	}
}

// A grid widget picks which of its grid's metrics it shows, in order: by
// name or id, under metric_ids or "metrics" (the key create_grid reads), on
// add and on update; a metric of another grid is refused.
func TestGridWidgetChosenMetrics(t *testing.T) {
	h := newEditHarness(t)
	_, rev := h.must("create_metric", map[string]any{"name": "revenue", "is_input": true})
	_, cost := h.must("create_metric", map[string]any{"name": "cost", "is_input": true})
	h.must("create_metric", map[string]any{"name": "stray", "is_input": true})
	_, grid := h.must("create_grid", map[string]any{"name": "G", "metric_ids": []string{rev, cost}})
	_, dash := h.must("create_dashboard", map[string]any{"name": "D"})
	_, w := h.must("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "grid", "ref_id": grid,
		"widget_props": map[string]any{"metrics": []string{"cost", "revenue"}}})
	if s := h.scalar(`SELECT widget_props->>'metric_ids' FROM model.dashboard_widget WHERE id=$1::uuid`, w); s != fmt.Sprintf(`["%s", "%s"]`, cost, rev) {
		t.Errorf("metric_ids = %s, want [cost revenue] as ids", s)
	}
	_, _, err := h.run("update_dashboard_widget", map[string]any{"widget_id": w, "widget_props": map[string]any{"metric_ids": []string{"stray"}}})
	h.refused("a metric the grid does not hold", err, "does not hold stray")
	h.must("update_dashboard_widget", map[string]any{"widget_id": w, "widget_props": map[string]any{"metrics": []string{"revenue"}}})
	if s := h.scalar(`SELECT widget_props->>'metric_ids' FROM model.dashboard_widget WHERE id=$1::uuid`, w); s != fmt.Sprintf(`["%s"]`, rev) {
		t.Errorf("after update: metric_ids = %s, want [revenue]", s)
	}
	if out, err := aiassistant.NewToolExecutor(h.pool, h.modelID, h.revID).Execute(context.Background(), "list_dashboards", nil); err != nil || !strings.Contains(out, "showing metrics revenue") {
		t.Errorf("list_dashboards does not say which metrics the widget shows: %v\n%s", err, out)
	}
}

// add_dashboard_widget checked the reference of KPI, chart and grid widgets
// only; the developer endpoint checks every type that references something.
func TestAddDashboardWidget_ChecksEveryReference(t *testing.T) {
	h := newEditHarness(t)
	otherModel := seedModel(t, h.pool)
	otherRev := seedRevision(t, h.pool, otherModel, "Other")
	foreignGrid := h.scalar(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'theirs') RETURNING id::text`, otherModel, otherRev)
	foreignForm := h.scalar(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid,$2::uuid,'theirs','theirs','[]') RETURNING id::text`, otherModel, otherRev)
	otherApp := h.scalar(`SELECT application_id::text FROM core.model WHERE id=$1::uuid`, otherModel)
	foreignRule := h.scalar(`INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name) VALUES ($1::uuid,'theirs','manual','x') RETURNING id::text`, otherApp)
	_, dash := h.must("create_dashboard", map[string]any{"name": "D"})
	for _, tc := range []struct{ typ, ref, want string }{
		{"import", foreignGrid, "different model"},
		{"form", foreignForm, "different model"},
		{"automation_button", foreignRule, "different application"},
	} {
		_, _, err := h.run("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": tc.typ, "ref_id": tc.ref})
		h.refused(tc.typ+" widget over another model's resource", err, tc.want)
	}
	_, _, err := h.run("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "image", "content": "not an image"})
	if err == nil {
		t.Error("an image widget with content that is not an image was accepted")
	}
}

// ── dimensions and members ────────────────────────────────────────────────────

func TestDeleteDimensionAndMember(t *testing.T) {
	h := newEditHarness(t)
	_, headcount := h.must("create_metric", map[string]any{"name": "headcount", "is_input": true})
	_, region := h.must("create_dimension", map[string]any{"name": "region", "members": []map[string]any{
		{"code": "EMEA", "label": "EMEA"}, {"code": "APAC", "label": "APAC"}, {"code": "UK", "label": "UK", "parent_code": "EMEA"}}})
	h.must("create_metric", map[string]any{"name": "emea_heads", "formula": `LOOKUP(headcount, region, "EMEA")`})
	h.fact(headcount, map[string]string{region: "APAC"}, 7)

	_, _, err := h.run("delete_dimension_member", map[string]any{"dimension_id": "region", "code": "EMEA"})
	h.refused("deleting a member a formula names", err, metricformula.CodeMemberInUse)
	_, _, err = h.run("delete_dimension", map[string]any{"dimension_id": "region"})
	h.refused("deleting a dimension a formula names", err, metricformula.CodeDimensionInUse)

	res, _ := h.must("delete_dimension_member", map[string]any{"dimension_id": "region", "code": "APAC"})
	if !strings.Contains(res, "history") {
		t.Errorf("result %q", res)
	}
	if n := h.scalar(`SELECT count(*)::text FROM runtime.fact_input WHERE metric_id=$1::uuid`, headcount); n != "0" {
		t.Error("the deleted member's input value was left behind")
	}
	if r := h.scalar(`SELECT delete_reason FROM runtime.fact_input_history WHERE metric_id=$1::uuid`, headcount); r != "member_deleted" {
		t.Errorf("history reason %q, want member_deleted", r)
	}

	h.must("delete_metric", map[string]any{"metric_id": "emea_heads"})
	res, _ = h.must("delete_dimension_member", map[string]any{"dimension_id": "region", "code": "EMEA"})
	if !strings.Contains(res, "1 child member(s) are now top-level") {
		t.Errorf("result %q", res)
	}
	h.must("delete_dimension", map[string]any{"dimension_id": region})
	if n := h.scalar(`SELECT count(*)::text FROM model.dimension_def WHERE id=$1::uuid`, region); n != "0" {
		t.Error("dimension not deleted")
	}
	// Another model's dimension is refused.
	other := seedModel(t, h.pool)
	theirs := h.scalar(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'x') RETURNING id::text`, other, seedRevision(t, h.pool, other, "O"))
	_, _, err = h.run("delete_dimension", map[string]any{"dimension_id": theirs})
	h.refused("another model's dimension", err, "different model")
}

// update_dimension_member's new_code carries the member's stored values under
// the new code, as the developer's PATCH does; a top-level member placed
// under a childless parent takes over the parent's values.
func TestUpdateDimensionMember_CodeRenameAndFirstChild(t *testing.T) {
	h := newEditHarness(t)
	_, revenue := h.must("create_metric", map[string]any{"name": "revenue", "is_input": true})
	_, region := h.must("create_dimension", map[string]any{"name": "region", "members": []map[string]any{
		{"code": "EMEA", "label": "EMEA"}, {"code": "UK", "label": "UK"}, {"code": "DE", "label": "DE"}}})
	h.fact(revenue, map[string]string{region: "UK"}, 10)
	h.fact(revenue, map[string]string{region: "EMEA"}, 99)

	h.must("update_dimension_member", map[string]any{"dimension_id": "region", "code": "UK", "new_code": "GB", "label": "Britain"})
	if c := h.scalar(`SELECT dim_members->>$1 FROM runtime.fact_input WHERE value=10`, region); c != "GB" {
		t.Errorf("fact still filed under %q after the rename", c)
	}
	_, _, err := h.run("update_dimension_member", map[string]any{"dimension_id": "region", "code": "GB", "new_code": "DE"})
	h.refused("renaming onto a taken code", err, metricformula.CodeMemberCodeTaken)

	// GB (top-level) goes under EMEA, which had no children: EMEA's 99 moves to GB.
	h.must("update_dimension_member", map[string]any{"dimension_id": "region", "code": "GB", "parent_code": "EMEA"})
	if v := h.scalar(`SELECT COALESCE(string_agg(value::text, ',' ORDER BY value), '') FROM runtime.fact_input WHERE dim_members->>$1 = 'EMEA'`, region); v != "" {
		t.Errorf("EMEA kept values %s after gaining its first child", v)
	}
	if v := h.scalar(`SELECT string_agg(value::text, ',' ORDER BY value) FROM runtime.fact_input WHERE dim_members->>$1 = 'GB'`, region); v != "10,99" {
		t.Errorf("GB values %s, want 10,99", v)
	}
	// add_dimension_member does the same for a new first child.
	h.fact(revenue, map[string]string{region: "DE"}, 5)
	h.must("add_dimension_member", map[string]any{"dimension_id": "region", "code": "BER", "label": "Berlin", "parent_code": "DE"})
	if v := h.scalar(`SELECT string_agg(value::text, ',') FROM runtime.fact_input WHERE dim_members->>$1 = 'BER'`, region); v != "5" {
		t.Errorf("BER values %q, want DE's 5", v)
	}
}

func TestTimeMembers_GenerateAndRedate(t *testing.T) {
	h := newEditHarness(t)
	h.must("create_dimension", map[string]any{"name": "month", "dimension_type": "time", "time_granularity": "month",
		"fiscal_year_start_month": 1, "members": []map[string]any{{"code": "FY26", "label": "FY26"}}})
	res, _ := h.must("generate_time_members", map[string]any{"dimension_id": "month", "start": "2026-01-01", "end": "2026-06-30", "parent_code": "FY26"})
	if !strings.Contains(res, "Generated 6") {
		t.Errorf("result %q", res)
	}
	res, _ = h.must("generate_time_members", map[string]any{"dimension_id": "month", "start": "2026-01-01", "end": "2026-07-31", "parent_code": "FY26"})
	if !strings.Contains(res, "Generated 1") || !strings.Contains(res, "6 already existed") {
		t.Errorf("re-run result %q", res)
	}
	codes := h.scalar(`SELECT string_agg(m.code || '@' || m.time_index, ',' ORDER BY m.time_index) FROM model.dimension_member m
		JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.name='month' AND m.period_start IS NOT NULL`)
	if !strings.HasSuffix(codes, "@6") || strings.Count(codes, ",") != 6 {
		t.Errorf("periods %s, want 7 indexed 0..6", codes)
	}
	var firstCode string
	firstCode = strings.SplitN(strings.SplitN(codes, ",", 2)[0], "@", 2)[0]

	// A label-only change keeps the leaf's dates (it does not become an aggregate).
	h.must("update_dimension_member", map[string]any{"dimension_id": "month", "code": firstCode, "label": "January"})
	if d := h.scalar(`SELECT COALESCE(period_start::text,'') FROM model.dimension_member WHERE code=$1`, firstCode); d != "2026-01-01" {
		t.Errorf("label change lost the dates: start %q", d)
	}
	// Re-dating onto a neighbour's dates is refused by the shared validator.
	_, _, err := h.run("update_dimension_member", map[string]any{"dimension_id": "month", "code": firstCode,
		"period_start": "2026-02-01", "period_end": "2026-02-28"})
	if err == nil {
		t.Error("an overlapping period was accepted")
	}
	_, _, err = h.run("generate_time_members", map[string]any{"dimension_id": "month", "start": "2026-01-01", "end": "2026-01-31", "parent_code": "nope"})
	h.refused("an unknown parent period", err, "not found")
}

// ── grids ─────────────────────────────────────────────────────────────────────

func TestGridEdits(t *testing.T) {
	h := newEditHarness(t)
	_, headcount := h.must("create_metric", map[string]any{"name": "headcount", "is_input": true})
	_, region := h.must("create_dimension", map[string]any{"name": "region", "members": []map[string]any{{"code": "EMEA", "label": "EMEA"}}})
	h.must("create_dimension", map[string]any{"name": "product"})
	_, grid := h.must("create_grid", map[string]any{"name": "Plan", "metric_ids": []string{headcount}, "dimension_ids": []string{"region", "product"}})
	// Other has region too: emea_share reads headcount along region, so
	// headcount may only move to a grid that keeps region.
	_, other := h.must("create_grid", map[string]any{"name": "Other", "dimension_ids": []string{"region"}})

	h.must("update_grid", map[string]any{"grid_id": "Plan", "name": "Headcount plan"})
	if n := h.scalar(`SELECT name FROM model.grid_def WHERE id=$1::uuid`, grid); n != "Headcount plan" {
		t.Errorf("name %q", n)
	}
	h.must("update_grid_dimension", map[string]any{"grid_id": grid, "dimension_id": "region", "display_level": 2})
	if l := h.scalar(`SELECT display_level::text FROM model.grid_dimension WHERE grid_id=$1::uuid AND dimension_id=$2::uuid`, grid, region); l != "2" {
		t.Errorf("display level %s", l)
	}
	h.must("update_grid_dimension", map[string]any{"grid_id": grid, "dimension_id": "region", "display_level": nil})

	// A metric on the grid reads region: removing region is refused.
	_, share := h.must("create_metric", map[string]any{"name": "emea_share", "formula": `SUMIFS(headcount, region, "EMEA")`})
	h.must("add_grid_metric", map[string]any{"grid_id": grid, "metric_id": share})
	if _, _, err := h.run("remove_grid_dimension", map[string]any{"grid_id": grid, "dimension_id": "region"}); err == nil {
		t.Error("removed a dimension a metric on the grid reads")
	}
	h.must("remove_grid_dimension", map[string]any{"grid_id": grid, "dimension_id": "product"})

	// Moving a metric: remove it from one grid, add it to another.
	_, _, err := h.run("add_grid_metric", map[string]any{"grid_id": other, "metric_id": headcount})
	h.refused("a metric already on a grid", err, "remove it there first")
	h.must("remove_grid_metric", map[string]any{"grid_id": grid, "metric_id": "headcount"})
	h.must("add_grid_metric", map[string]any{"grid_id": other, "metric_id": headcount})
	_, _, err = h.run("remove_grid_metric", map[string]any{"grid_id": grid, "metric_id": headcount})
	h.refused("removing a metric the grid does not have", err, "not on this grid")

	_, dash := h.must("create_dashboard", map[string]any{"name": "D"})
	h.must("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "grid", "ref_id": other})
	h.must("delete_grid", map[string]any{"grid_id": other})
	if n := h.scalar(`SELECT count(*)::text FROM model.dashboard_widget WHERE dashboard_id=$1::uuid`, dash); n != "0" {
		t.Error("a widget showing the deleted grid was left behind")
	}
}

// ── dashboards, widgets, folders ──────────────────────────────────────────────

func TestDashboardFolderAndWidgetEdits(t *testing.T) {
	h := newEditHarness(t)
	_, revenue := h.must("create_metric", map[string]any{"name": "revenue", "is_input": true})
	_, finance := h.must("create_dashboard_folder", map[string]any{"name": "Finance"})
	_, sub := h.must("create_dashboard_folder", map[string]any{"name": "Monthly", "parent": "Finance"})
	_, _, err := h.run("update_dashboard_folder", map[string]any{"folder": finance, "parent": "Monthly"})
	h.refused("a folder moved inside itself", err, "inside itself")

	_, dash := h.must("create_dashboard", map[string]any{"name": "Overview", "folder": "Monthly"})
	if f := h.scalar(`SELECT folder_id::text FROM model.dashboard_def WHERE id=$1::uuid`, dash); f != sub {
		t.Errorf("created in folder %s, want Monthly", f)
	}
	h.must("update_dashboard", map[string]any{"dashboard_id": "Overview", "name": "Board", "folder": nil})
	if f := h.scalar(`SELECT name || '|' || COALESCE(folder_id::text,'') FROM model.dashboard_def WHERE id=$1::uuid`, dash); f != "Board|" {
		t.Errorf("after update: %s", f)
	}

	_, w := h.must("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "metric_kpi", "ref_id": revenue,
		"pos_x": 0, "pos_y": 0, "size_w": 300, "size_h": 120})
	h.must("update_dashboard_widget", map[string]any{"widget_id": w, "pos_y": 160, "title": "Revenue", "size_w": 5})
	if s := h.scalar(`SELECT pos_x || ',' || pos_y || ',' || size_w || ',' || size_h || ',' || title FROM model.dashboard_widget WHERE id=$1::uuid`, w); s != "0,160,20,120,Revenue" {
		t.Errorf("widget after partial update: %s (untouched fields kept, size clamped to 20)", s)
	}
	out, err := aiassistant.NewToolExecutor(h.pool, h.modelID, h.revID).Execute(h.ctx, "list_dashboards", nil)
	if err != nil || !strings.Contains(out, "widget (id:"+w+") metric_kpi → revenue at (0,160) size 20x120") || !strings.Contains(out, "Monthly (id:"+sub+", in Finance)") {
		t.Errorf("list_dashboards: %v\n%s", err, out)
	}
	h.must("delete_dashboard_widget", map[string]any{"widget_id": w})

	h.must("update_dashboard", map[string]any{"dashboard_id": dash, "folder": "Monthly"})
	h.must("delete_dashboard_folder", map[string]any{"folder": "Finance"})
	if s := h.scalar(`SELECT (SELECT count(*) FROM model.dashboard_folder WHERE model_id=$1::uuid)::text || '|' ||
		COALESCE((SELECT folder_id::text FROM model.dashboard_def WHERE id=$2::uuid), '')`, h.modelID, dash); s != "0|" {
		t.Errorf("after deleting the parent folder: %s, want no folders and the dashboard at the top level", s)
	}
	h.must("delete_dashboard", map[string]any{"dashboard_id": "Board"})

	// Another model's folder is refused.
	other := seedModel(t, h.pool)
	theirs := h.scalar(`INSERT INTO model.dashboard_folder (model_id, name, revision_id) VALUES ($1::uuid,'x',$2::uuid) RETURNING id::text`, other, seedRevision(t, h.pool, other, "O"))
	_, _, err = h.run("create_dashboard", map[string]any{"name": "Sneaky", "folder": theirs})
	h.refused("another model's folder", err, "different model")
}

// A widget id the assistant listed before its draft existed belongs to the
// active revision; it resolves to the draft's copy of the widget, never to
// the original.
func TestWidgetIDsResolveIntoTheDraft(t *testing.T) {
	h := newEditHarness(t)
	h.scalar(`UPDATE core.model SET active_revision_id=$2::uuid WHERE id=$1::uuid RETURNING id::text`, h.modelID, h.revID)
	_, dash := h.must("create_dashboard", map[string]any{"name": "D"})
	_, w := h.must("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "text", "content": "hi", "pos_x": 10, "pos_y": 20, "size_w": 200, "size_h": 60})

	unscoped := aiassistant.NewWriteExecutor(h.pool, h.modelID, "")
	_, draft, err := unscoped.Execute(h.ctx, "create_revision", mustJSON(t, map[string]any{"name": "AI Draft"}))
	if err != nil {
		t.Fatal(err)
	}
	inDraft := aiassistant.NewWriteExecutor(h.pool, h.modelID, draft)
	if _, _, err := inDraft.Execute(h.ctx, "update_dashboard_widget", mustJSON(t, map[string]any{"widget_id": w, "pos_y": 300})); err != nil {
		t.Fatal(err)
	}
	if y := h.scalar(`SELECT pos_y::text FROM model.dashboard_widget WHERE id=$1::uuid`, w); y != "20" {
		t.Errorf("the active revision's widget moved to %s", y)
	}
	if y := h.scalar(`SELECT w.pos_y::text FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id WHERE d.revision_id=$1::uuid`, draft); y != "300" {
		t.Errorf("the draft's copy is at %s, want 300", y)
	}
}

// ── workflows, roles, form integrations ───────────────────────────────────────

func TestWorkflowLifecycle(t *testing.T) {
	h := newEditHarness(t)
	h.exec = aiassistant.NewWriteExecutorWithActor(h.pool, h.modelID, h.revID, h.user())
	h.must("create_workflow_def", map[string]any{"name": "Approval", "trigger_event": "manual"})
	h.must("archive_workflow_def", map[string]any{"workflow_def_id": "Approval"})
	if s := h.scalar(`SELECT status FROM workflow.workflow_def WHERE name='Approval'`); s != "archived" {
		t.Errorf("status %s after archive", s)
	}
	h.must("restore_workflow_def", map[string]any{"workflow_def_id": "Approval"})
	if s := h.scalar(`SELECT status FROM workflow.workflow_def WHERE name='Approval'`); s != "draft" {
		t.Errorf("status %s after restore", s)
	}
	_, dup := h.must("duplicate_workflow_def", map[string]any{"workflow_def_id": "Approval", "name": "Approval v2"})
	if r := h.scalar(`SELECT COALESCE(revision_id::text,'') || '|' || status FROM workflow.workflow_def WHERE id=$1::uuid`, dup); r != h.revID+"|draft" {
		t.Errorf("duplicate %s, want a draft in the working revision", r)
	}
	_, _, err := h.run("duplicate_workflow_def", map[string]any{"workflow_def_id": "Approval", "name": "Approval v2"})
	if err == nil {
		t.Error("a duplicate under a taken name was accepted")
	}
}

func TestBusinessRoleEdits(t *testing.T) {
	h := newEditHarness(t)
	h.exec = aiassistant.NewWriteExecutorWithActor(h.pool, h.modelID, h.revID, h.user())
	_, role := h.must("create_business_role", map[string]any{"name": "Finance Review"})
	h.must("create_business_role", map[string]any{"name": "Auditors"})
	h.must("create_workflow_def", map[string]any{"name": "Expense", "trigger_event": "manual"})
	h.must("update_workflow_def", map[string]any{"workflow_def_id": "Expense", "steps": []map[string]any{
		{"id": "a", "name": "Review", "type": "approval", "assignee_roles": []string{"Finance Review"},
			"routes": map[string]string{"approve": "end-completed", "reject": "end-rejected"}}}})

	res, _ := h.must("update_business_role", map[string]any{"role": "finance review", "name": "Finance"})
	if !strings.Contains(res, "Expense") {
		t.Errorf("rename result %q does not name the workflow that still names the old role", res)
	}
	_, _, err := h.run("update_business_role", map[string]any{"role": "Finance", "name": "Auditors"})
	h.refused("renaming onto a taken name", err, "already exists")

	_, d1 := h.must("create_dashboard", map[string]any{"name": "Overview"})
	h.must("create_dashboard", map[string]any{"name": "Costs"})
	other := seedRevision(t, h.pool, h.modelID, "Rev B")
	elsewhere := h.scalar(`INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'Overview') RETURNING id::text`, h.modelID, other)
	h.scalar(`INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid,$2::uuid) RETURNING role_id::text`, role, elsewhere)
	h.must("set_role_dashboards", map[string]any{"role": role, "dashboards": []string{"Overview", "Costs"}})
	h.must("set_role_dashboards", map[string]any{"role": role, "dashboards": []string{d1}})
	grants := h.scalar(`SELECT string_agg(d.name || '@' || r.name, ',' ORDER BY r.name) FROM identity.business_role_dashboard brd
		JOIN model.dashboard_def d ON d.id=brd.dashboard_id JOIN model.revision r ON r.id=d.revision_id WHERE brd.role_id=$1::uuid`, role)
	if grants != "Overview@Rev A,Overview@Rev B" {
		t.Errorf("grants %s: the working revision's set replaced, another revision's kept", grants)
	}
	out, _ := aiassistant.NewToolExecutor(h.pool, h.modelID, h.revID).Execute(h.ctx, "list_workflow_roles", nil)
	if !strings.Contains(out, "Finance (id:"+role+", 0 member(s); may open dashboards of this revision: Overview)") {
		t.Errorf("list_workflow_roles:\n%s", out)
	}
	h.must("delete_business_role", map[string]any{"role": "Finance"})
	if n := h.scalar(`SELECT count(*)::text FROM identity.business_role WHERE id=$1::uuid`, role); n != "0" {
		t.Error("role not deleted")
	}
}

func TestBackfillFormIntegration_UsesTheGatewayPosting(t *testing.T) {
	h := newEditHarness(t)
	var posted []string
	h.exec.WithHooks(aiassistant.Hooks{PostFormIntegration: func(_ context.Context, id string) (int, error) {
		posted = append(posted, id)
		return 3, nil
	}})
	formID := h.scalar(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid,$2::uuid,'Expenses','Expenses','[]') RETURNING id::text`, h.modelID, h.revID)
	_, metric := h.must("create_metric", map[string]any{"name": "spend", "is_input": true})
	mapping := h.scalar(`INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, name, source_field, target_metric_id)
		VALUES ($1::uuid,$2::uuid,$3::uuid,'post','amount',$4::uuid) RETURNING id::text`, h.modelID, h.revID, formID, metric)
	res, _ := h.must("backfill_form_integration", map[string]any{"form_integration_id": mapping})
	if !strings.Contains(res, "Posted 3") || len(posted) != 1 || posted[0] != mapping {
		t.Errorf("result %q, hook calls %v", res, posted)
	}
	h.exec.WithHooks(aiassistant.Hooks{})
	_, _, err := h.run("backfill_form_integration", map[string]any{"form_integration_id": mapping})
	h.refused("backfill without the gateway's posting", err, "not available")
}

// Calculated members through the AI Developer's tools, as the console sets
// them: on create_dimension's members and add/update_dimension_member, shown
// by list_dimensions, refused when the formula reads no sibling.
func TestAICalculatedMembers(t *testing.T) {
	h := newEditHarness(t)
	_, dim := h.must("create_dimension", map[string]any{"name": "scenario", "members": []map[string]any{
		{"code": "RF", "label": "Forecast"}, {"code": "LY", "label": "Prior year"},
		{"code": "VAR", "label": "Variance", "formula": "{RF} - {LY}"}}})
	h.must("add_dimension_member", map[string]any{"dimension_id": dim, "code": "VARPCT", "label": "Variance %",
		"formula": "IF({LY} = 0, 0, ({RF} - {LY}) / ABS({LY}) * 100)"})
	h.must("update_dimension_member", map[string]any{"dimension_id": dim, "code": "VAR", "formula": "{LY} - {RF}"})
	if f := h.scalar(`SELECT formula FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='VAR'`, dim); f != "{LY} - {RF}" {
		t.Errorf("VAR = %q after update_dimension_member", f)
	}
	_, _, err := h.run("update_dimension_member", map[string]any{"dimension_id": dim, "code": "VAR", "formula": "{RF} - {BUDGET}"})
	h.refused("a formula reading no member", err, "not a member of scenario")
	out, err := aiassistant.NewToolExecutor(h.pool, h.modelID, h.revID).Execute(context.Background(), "list_dimensions", nil)
	if err != nil || !strings.Contains(out, "= {LY} - {RF} (calculated member)") {
		t.Errorf("list_dimensions does not show the formula: %v\n%s", err, out)
	}
}

// A widget's props are checked, not stored blind: live, a KPI tile scoped
// {"Scenario": "RF", "Month": "FY2026"} with mode "total" saved and showed the
// whole model, and a chart's "pin" did nothing. A scope names its dimension
// by id or name and pins the tile.
func TestAIWidgetPropsAreChecked(t *testing.T) {
	h := newEditHarness(t)
	_, dim := h.must("create_dimension", map[string]any{"name": "scenario", "members": []map[string]any{{"code": "RF", "label": "RF"}, {"code": "LY", "label": "LY"}}})
	_, rev := h.must("create_metric", map[string]any{"name": "revenue", "is_input": true})
	_, grid := h.must("create_grid", map[string]any{"name": "G", "metric_ids": []string{rev}, "dimension_ids": []string{dim}})
	_, dash := h.must("create_dashboard", map[string]any{"name": "D"})
	kpi := func(props map[string]any) error {
		_, _, err := h.run("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "metric_kpi", "ref_id": rev, "widget_props": props})
		return err
	}
	h.refused("a scope map", kpi(map[string]any{"kpi_scope": map[string]any{"Scenario": "RF"}}), `kpi_scope is {"dimension_id"`)
	h.refused("a scope with mode total", kpi(map[string]any{"kpi_scope": map[string]any{"dimension_id": "scenario", "member_code": "RF"}, "kpi_context_mode": "total"}), "contradicts kpi_scope")
	_, _, err := h.run("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "chart", "ref_id": grid,
		"widget_props": map[string]any{"pin": map[string]any{"scenario": "RF"}, "chart": map[string]any{"chart_type": "bar", "metric_ids": []string{rev}}}})
	h.refused("an unknown key", err, `no key "pin"`)
	_, w := h.must("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "metric_kpi", "ref_id": rev, "pos_y": 200,
		"widget_props": map[string]any{"kpi_scope": map[string]any{"dimension_id": "scenario", "member_code": "RF"}}})
	if got := h.scalar(`SELECT widget_props->>'kpi_context_mode' || ' ' || (widget_props->'kpi_scope'->>'dimension_id') FROM model.dashboard_widget WHERE id=$1::uuid`, w); got != "pin "+dim {
		t.Errorf("stored mode and scope dimension = %q, want pin and the dimension's id", got)
	}

	// show_members: dimension by name, stored by id; codes, not labels.
	gridWidget := func(props map[string]any) (string, error) {
		_, id, err := h.run("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "grid", "ref_id": grid, "pos_y": 400, "widget_props": props})
		return id, err
	}
	_, err = gridWidget(map[string]any{"show_members": map[string]any{"scenario": []string{"RF", "Forecast"}}})
	h.refused("a label for a code", err, `"Forecast" is not a member code`)
	gw, err := gridWidget(map[string]any{"show_members": map[string]any{"scenario": []string{"LY", "RF"}}})
	if err != nil {
		t.Fatalf("grid widget with show_members: %v", err)
	}
	if got := h.scalar(`SELECT widget_props->'show_members'->>$2::text FROM model.dashboard_widget WHERE id=$1::uuid`, gw, dim); got != `["LY", "RF"]` {
		t.Errorf("stored show_members for the dimension's id = %q, want [LY, RF] in order", got)
	}
}
