package modeltransfer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// minimalPackage is a package as a person might write it by hand, or as an
// older export looks: every optional field left out (and a few JSON values an
// explicit null), so nothing but ids, names and references is given.
const minimalPackage = `{
  "model_name": "Hand-made",
  "dimensions": [
    {"id": "dim-region", "name": "Region",
     "typed_properties": [{"name": "manager"}],
     "members": [{"id": "mem-north", "code": "N", "label": "North", "properties": null}]},
    {"id": "dim-month", "name": "Month", "dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1,
     "members": [
       {"id": "mem-feb", "code": "2026-02", "label": "Feb 2026", "period_start": "2026-02-01", "period_end": "2026-02-28"},
       {"id": "mem-jan", "code": "2026-01", "label": "Jan 2026", "period_start": "2026-01-01", "period_end": "2026-01-31"}]}
  ],
  "metrics": [{"id": "met-rev", "name": "Revenue", "is_input": true}],
  "grids": [{"id": "grid-plan", "name": "Plan", "metrics": [{"metric_id": "met-rev"}], "dimensions": [{"dimension_id": "dim-region"}]}],
  "forms": [{"id": "form-req", "name": "request", "label": "Request", "fields": null}],
  "form_records": [{"form_id": "form-req", "data": {"amount": 5}}],
  "form_mappings": [{"id": "map-amt", "form_id": "form-req", "name": "Amount", "source_field": "amount", "target_metric_id": "met-rev"}],
  "workflows": [{"id": "wf-approve", "name": "Approve", "trigger_event": "manual"}],
  "automation_rules": [{"id": "rule-run", "name": "Run approval", "workflow_name": "Approve", "workflow_def_id": "wf-approve"}],
  "integrations": [{"id": "int-feed", "name": "Feed", "target_id": "grid-plan", "config": null}],
  "dashboards": [{"id": "dash-over", "name": "Overview", "widgets": [
    {"widget_type": "metric_kpi", "ref_id": "met-rev"},
    {"widget_type": "text", "content": "Placed", "pos_x": 0, "pos_y": 0, "size_w": 300, "size_h": 100},
    {"widget_type": "grid", "ref_id": "grid-plan", "col_start": 7, "col_span": 6, "sort_order": 3},
    {"widget_type": "text", "content": "Half placed", "pos_y": 400, "size_w": 500}
  ]}],
  "facts": [{"metric_id": "met-rev", "dim_members": {"dim-region": "N"}, "value": 5}]
}`

// A package that leaves optional fields out must import, and every field it
// left out must come out as the column's own default — the value the engine
// gives a row created without it — not as a NULL the column rejects, a JSON
// null, or an empty string. The defaults are read from the live schema, so
// the literals Import uses for them cannot drift from the migrations.
func TestImportMinimalPackage(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)

	var pkg Package
	if err := json.Unmarshal([]byte(minimalPackage), &pkg); err != nil {
		t.Fatalf("decode package: %v", err)
	}
	var revID string
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		_, revID, err = Import(ctx, tx, ImportRequest{ApplicationID: app, Package: pkg}, user)
		return err
	}); err != nil {
		t.Fatalf("import minimal package: %v", err)
	}

	const (
		byRev  = `revision_id=$1::uuid`
		byDim  = `dimension_id IN (SELECT id FROM model.dimension_def WHERE revision_id=$1::uuid)`
		byForm = `form_id IN (SELECT id FROM model.form_def WHERE revision_id=$1::uuid)`
		byDash = `dashboard_id IN (SELECT id FROM model.dashboard_def WHERE revision_id=$1::uuid) AND widget_type='metric_kpi'`
	)
	for _, c := range []struct{ table, column, where string }{
		{"model.dimension_def", "agg_rule", byRev},
		{"model.dimension_def", "properties", byRev},
		{"model.dimension_def", "tags", byRev},
		{"model.dimension_member", "properties", byDim},
		{"model.dimension_property", "data_type", byDim},
		{"model.metric_def", "storage_type", byRev},
		{"model.metric_def", "agg_rule", byRev},
		{"model.metric_def", "format", byRev},
		{"model.metric_def", "format_currency", byRev},
		{"model.metric_def", "time_summary", byRev},
		{"model.metric_def", "tags", byRev},
		{"model.form_def", "fields", byRev},
		{"runtime.form_record", "status", byForm},
		{"model.form_metric_mapping", "aggregation", byRev},
		{"model.form_metric_mapping", "posting_statuses", byRev},
		{"model.form_metric_mapping", "dimension_mappings", byRev},
		{"model.form_metric_mapping", "live_posting", byRev},
		{"model.dashboard_def", "tags", byRev},
		{"model.dashboard_def", "category", byRev},
		{"model.dashboard_widget", "size_h", byDash},
		{"model.dashboard_widget", "col_start", byDash},
		{"model.dashboard_widget", "col_span", byDash},
		{"workflow.workflow_def", "steps", byRev},
		{"workflow.workflow_def", "context_schema", byRev},
		{"workflow.workflow_def", "subject_config", byRev},
		{"workflow.workflow_def", "status", byRev},
		{"workflow.automation_rule", "trigger_type", byRev},
		{"workflow.automation_rule", "enabled", byRev},
		{"model.integration_def", "type", byRev},
		{"model.integration_def", "target_type", byRev},
		{"model.integration_def", "config", byRev},
	} {
		want := columnDefault(t, ctx, pool, c.table, c.column)
		got := texts(t, ctx, pool, fmt.Sprintf(`SELECT %s::text FROM %s WHERE %s`, c.column, c.table, c.where), revID)
		if len(got) == 0 {
			t.Errorf("%s: no imported row to check", c.table)
		}
		for _, g := range got {
			if g != want {
				t.Errorf("%s.%s = %q, want the column default %q", c.table, c.column, g, want)
			}
		}
	}

	// The geometry derivation leans on two column defaults of its own.
	if d := columnDefault(t, ctx, pool, "model.dashboard_widget", "col_span"); d != strconv.Itoa(legacyColumns) {
		t.Errorf("col_span default = %s, widgetGeometry assumes %d", d, legacyColumns)
	}
	if d := columnDefault(t, ctx, pool, "model.dashboard_widget", "size_h"); d != strconv.Itoa(widgetHeightPx) {
		t.Errorf("size_h default = %s, widgetGeometry assumes %d", d, widgetHeightPx)
	}

	checkMinimalWidgets(t, ctx, pool, revID)

	// A dated member left without its ordinal is numbered with the rest.
	if got := strings.Join(texts(t, ctx, pool, `
		SELECT m.code || '=' || m.time_index FROM model.dimension_member m
		JOIN model.dimension_def d ON d.id = m.dimension_id
		WHERE d.revision_id=$1::uuid AND d.name='Month' ORDER BY m.time_index`, revID), " "); got != "2026-01=0 2026-02=1" {
		t.Errorf("time members = %q, want 2026-01=0 2026-02=1", got)
	}
	// An integration without a target_type targets a grid (the column
	// default), so its target is remapped as one.
	if got := texts(t, ctx, pool, `
		SELECT (i.target_id = g.id)::text FROM model.integration_def i
		JOIN model.grid_def g ON g.revision_id = i.revision_id
		WHERE i.revision_id=$1::uuid`, revID); len(got) != 1 || got[0] != "true" {
		t.Errorf("integration target remapped to the imported grid = %v, want [true]", got)
	}
	if got := texts(t, ctx, pool, `
		SELECT (f.dim_members ? d.id::text)::text FROM runtime.fact_input f
		JOIN model.dimension_def d ON d.revision_id = f.revision_id AND d.name='Region'
		WHERE f.revision_id=$1::uuid`, revID); len(got) != 1 || got[0] != "true" {
		t.Errorf("fact keyed by the imported Region dimension = %v, want [true]", got)
	}
}

// Widgets the package did not place are laid out from what it did say
// (legacy col_start/col_span, the other geometry fields) and stacked below
// everything placed, in package order — never piled over each other.
func checkMinimalWidgets(t *testing.T, ctx context.Context, pool *pgxpool.Pool, revID string) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT w.widget_type || '|' || COALESCE(w.content,''), w.pos_x, w.pos_y, w.size_w, w.size_h
		FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id = w.dashboard_id
		WHERE d.revision_id=$1::uuid`, revID)
	if err != nil {
		t.Fatalf("widgets: %v", err)
	}
	got := map[string]widgetBox{}
	for rows.Next() {
		var key string
		var b widgetBox
		if err := rows.Scan(&key, &b.x, &b.y, &b.w, &b.h); err != nil {
			t.Fatalf("widgets: %v", err)
		}
		got[key] = b
	}
	rows.Close()
	want := map[string]widgetBox{
		"text|Placed":      {0, 0, 300, 100},                   // as given
		"text|Half placed": {0, 400, 500, widgetHeightPx},      // given y and width kept
		"metric_kpi|":      {0, 620, legacyColumns * 100, 200}, // below the lowest placed (400+200) + gap
		"grid|":            {600, 840, 600, widgetHeightPx},    // col_start 7 / col_span 6, stacked next
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("widget %s = %+v, want %+v", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("imported %d widgets, want %d", len(got), len(want))
	}
	var boxes []string
	for k := range got {
		boxes = append(boxes, k)
	}
	for i, a := range boxes {
		for _, b := range boxes[i+1:] {
			p, q := got[a], got[b]
			if p.x < q.x+q.w && q.x < p.x+p.w && p.y < q.y+q.h && q.y < p.y+p.h {
				t.Errorf("widgets %s %+v and %s %+v overlap", a, p, b, q)
			}
		}
	}
}

// A time dimension without the settings every time dimension needs is
// refused with the rule it breaks, as the dimension writers refuse it.
func TestImportRejectsIncompleteTimeDimension(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	pkg := Package{ModelName: "Bad time", Dimensions: []Dimension{{ID: "d", Name: "Month", DimensionType: "time"}}}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, _, err := Import(ctx, tx, ImportRequest{ApplicationID: app, Package: pkg}, user)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "time_granularity is required") {
		t.Fatalf("import error = %v, want the missing time_granularity named", err)
	}
}

// A package is held to the rules every writer of the same rows applies, so
// what it leaves out or gets wrong is refused with the rule it breaks
// instead of importing into a state the engine forbids.
func TestImportRejectsForbiddenState(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	str := func(s string) *string { return &s }
	for _, c := range []struct {
		name string
		pkg  Package
		want string
	}{
		{"standard dimension with dated members", Package{ModelName: "Dated", Dimensions: []Dimension{{
			ID: "d", Name: "Month", // dimension_type left out: standard
			Members: []Member{{ID: "m", Code: "2026-01", Label: "Jan", PeriodStart: str("2026-01-01"), PeriodEnd: str("2026-01-31")}},
		}}}, "a standard dimension's members cannot carry period dates"},
		{"rate without operands", Package{ModelName: "Rate", Metrics: []Metric{
			{ID: "r", Name: "margin", AggRule: "rate"},
		}}, "needs both a numerator and a denominator"},
		{"rate operand outside the package", Package{ModelName: "Rate", Metrics: []Metric{
			{ID: "a", Name: "profit", IsInput: true},
			{ID: "r", Name: "margin", AggRule: "rate", AggNumeratorMetricID: str("a"), AggDenominatorMetricID: str("elsewhere")},
		}}, `denominator "elsewhere" is not in the package`},
	} {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			_, _, err := Import(ctx, tx, ImportRequest{ApplicationID: app, Package: c.pkg}, user)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: import error = %v, want %q", c.name, err, c.want)
		}
	}
}

// A rate metric's operands travel with it and are remapped to the imported
// metrics, not dropped (a rate with no operands) or left pointing at the
// source model's.
func TestImportRemapsRateOperands(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	str := func(s string) *string { return &s }
	pkg := Package{ModelName: "Rate", Metrics: []Metric{
		{ID: "p", Name: "profit", IsInput: true},
		{ID: "v", Name: "revenue", IsInput: true},
		{ID: "r", Name: "margin", Formula: str("=profit/revenue"), AggRule: "rate",
			AggNumeratorMetricID: str("p"), AggDenominatorMetricID: str("v")},
	}}
	var revID string
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		_, revID, err = Import(ctx, tx, ImportRequest{ApplicationID: app, Package: pkg}, user)
		return err
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	got := texts(t, ctx, pool, `
		SELECT r.name || '=' || n.name || '/' || d.name
		FROM model.metric_def r
		JOIN model.metric_def n ON n.id = r.agg_numerator_metric_id AND n.revision_id = r.revision_id
		JOIN model.metric_def d ON d.id = r.agg_denominator_metric_id AND d.revision_id = r.revision_id
		WHERE r.revision_id=$1::uuid`, revID)
	if strings.Join(got, ",") != "margin=profit/revenue" {
		t.Errorf("rate operands = %q, want margin=profit/revenue within the imported revision", got)
	}

	// And they survive the trip back out.
	var modelID string
	if err := pool.QueryRow(ctx, `SELECT model_id::text FROM model.revision WHERE id=$1::uuid`, revID).Scan(&modelID); err != nil {
		t.Fatalf("model of revision: %v", err)
	}
	exported, err := CollectExport(ctx, pool, modelID, revID, "Imported")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	names := map[string]string{}
	for _, m := range exported.Metrics {
		names[m.ID] = m.Name
	}
	for _, m := range exported.Metrics {
		if m.Name != "margin" {
			continue
		}
		if m.AggNumeratorMetricID == nil || m.AggDenominatorMetricID == nil ||
			names[*m.AggNumeratorMetricID] != "profit" || names[*m.AggDenominatorMetricID] != "revenue" {
			t.Errorf("exported margin operands = %v / %v, want profit / revenue", m.AggNumeratorMetricID, m.AggDenominatorMetricID)
		}
	}
}

// importTarget creates the application and importing user an import needs.
func importTarget(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (app, user string) {
	t.Helper()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Import Co', 'community') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	app = q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Imports', 'planning') RETURNING id::text`, cust, ws)
	user = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('importer', 'importer@import.test', 'Importer', $1::uuid) RETURNING id::text`, cust)
	return app, user
}

// columnDefault evaluates table.column's declared default, as text.
func columnDefault(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, column string) string {
	t.Helper()
	schema, name, _ := strings.Cut(table, ".")
	var expr *string
	if err := pool.QueryRow(ctx, `
		SELECT column_default FROM information_schema.columns
		WHERE table_schema=$1 AND table_name=$2 AND column_name=$3`, schema, name, column).Scan(&expr); err != nil {
		t.Fatalf("default of %s.%s: %v", table, column, err)
	}
	if expr == nil {
		t.Fatalf("%s.%s has no default", table, column)
	}
	var v string
	if err := pool.QueryRow(ctx, `SELECT (`+*expr+`)::text`).Scan(&v); err != nil {
		t.Fatalf("evaluate default %s of %s.%s: %v", *expr, table, column, err)
	}
	return v
}

func texts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out
}
