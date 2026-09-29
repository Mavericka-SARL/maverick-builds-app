package modeltransfer

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// referencePackage is a model in which every kind of reference a package
// can hold is used at least once: a dimension's parent and source, a
// member's parent, a grid's rollup source, a folder's parent, a dashboard's
// folder, a mapping's grid and dimension keys, form fields, a workflow's
// subject and context schema, an automation rule's workflow and form,
// integration targets (a grid, and a connector's dimension named again in
// its config), every widget type that has a ref_id, widget_props'
// dimension and metric IDs, and a fact's dimension keys. Its IDs are not
// UUIDs, as in a hand-written package; its export (the same model with the
// UUIDs a database gives it) covers the other shape.
const referencePackage = `{
  "model_name": "References",
  "dimensions": [
    {"id": "dim-region", "name": "Region", "typed_properties": [{"name": "zone", "data_type": "text"}],
     "members": [
       {"id": "mem-all", "code": "ALL", "label": "All"},
       {"id": "mem-north", "code": "N", "label": "North", "parent_member_id": "mem-all", "properties": {"zone": "Z1"}}]},
    {"id": "dim-zone", "name": "Zone", "source_dimension_id": "dim-region", "source_property": "zone", "members": []},
    {"id": "dim-city", "name": "City", "parent_dimension_id": "dim-region",
     "members": [{"id": "mem-oslo", "code": "OSL", "label": "Oslo"}]}
  ],
  "metrics": [
    {"id": "met-rev", "name": "Revenue", "is_input": true},
    {"id": "met-cost", "name": "Cost", "is_input": true}
  ],
  "grids": [
    {"id": "grid-plan", "name": "Plan", "metrics": [{"metric_id": "met-rev"}], "dimensions": [{"dimension_id": "dim-region"}]},
    {"id": "grid-sum", "name": "Summary", "rollup_source_grid_id": "grid-plan", "metrics": [{"metric_id": "met-cost"}], "dimensions": []}
  ],
  "forms": [{"id": "form-req", "name": "request", "label": "Request", "fields": [
    {"name": "region", "type": "dimension", "dimension_id": "dim-region"},
    {"name": "amount", "type": "number", "metric_id": "met-rev"}]}],
  "form_mappings": [{"id": "map-amt", "form_id": "form-req", "grid_id": "grid-plan", "name": "Amount",
    "source_field": "amount", "target_metric_id": "met-rev", "dimension_mappings": {"dim-region": "region"}}],
  "folders": [{"id": "fold-root", "name": "Root"}, {"id": "fold-sub", "name": "Sub", "parent_id": "fold-root"}],
  "workflows": [{"id": "wf-approve", "name": "Approve", "trigger_event": "manual", "subject_type": "form",
    "subject_config": {"form_id": "form-req"},
    "context_schema": [{"key": "region", "type": "dimension_member", "dimension_id": "dim-region"}]}],
  "automation_rules": [{"id": "rule-run", "name": "Run approval", "workflow_name": "Approve",
    "workflow_def_id": "wf-approve", "source_form_id": "form-req"}],
  "integrations": [
    {"id": "int-feed", "name": "Feed", "target_type": "grid", "target_id": "grid-plan", "config": {}},
    {"id": "int-api", "name": "Cities", "type": "rest_api", "target_type": "dimension", "target_id": "dim-city",
     "config": {"kind": "rest_api/v1", "direction": "pull", "target_type": "dimension", "target_id": "dim-city",
                "request": {"method": "GET", "url": "https://api.example.test/cities"}}}
  ],
  "dashboards": [{"id": "dash-over", "name": "Overview", "folder_id": "fold-sub", "widgets": [
    {"widget_type": "grid", "ref_id": "grid-plan", "widget_props": {"default_view": {
      "rows": ["dim-region"], "cols": ["__metrics__"], "context": [], "filter_sel": {"dim-region": "N"}}}},
    {"widget_type": "chart", "ref_id": "grid-plan", "sort_order": 1, "widget_props": {"chart": {
      "dimension_id": "dim-region", "metric_ids": ["met-rev", "met-cost"], "context_defaults": {"dim-region": "N"}}}},
    {"widget_type": "metric_kpi", "ref_id": "met-rev", "sort_order": 2, "widget_props": {"kpi_scope": {"dimension_id": "dim-region", "member_code": "N"}}},
    {"widget_type": "form", "ref_id": "form-req", "sort_order": 3},
    {"widget_type": "import", "ref_id": "grid-plan", "sort_order": 4},
    {"widget_type": "integration_button", "ref_id": "int-feed", "sort_order": 5},
    {"widget_type": "automation_button", "ref_id": "rule-run", "sort_order": 6, "widget_props": {"context": {"region": "N"}}}
  ]}],
  "facts": [{"metric_id": "met-rev", "dim_members": {"dim-region": "N"}, "value": 5}]
}`

func decodePackage(t *testing.T, raw string) Package {
	t.Helper()
	var pkg Package
	if err := json.Unmarshal([]byte(raw), &pkg); err != nil {
		t.Fatalf("decode package: %v", err)
	}
	return pkg
}

func importInto(ctx context.Context, pool *pgxpool.Pool, app, user string, pkg Package) (modelID, revID string, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		modelID, revID, err = Import(ctx, tx, ImportRequest{ApplicationID: app, Package: pkg}, user)
		return err
	})
	return modelID, revID, err
}

// tenantTarget is importTarget for a second, separate tenant.
func tenantTarget(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) (app, user string) {
	t.Helper()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ($1, 'test') RETURNING id::text`, name)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	app = q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, $3, 'planning') RETURNING id::text`, cust, ws, name)
	user = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1 || '@import.test', $1, $2::uuid) RETURNING id::text`,
		strings.ToLower(strings.ReplaceAll(name, " ", "-")), cust)
	return app, user
}

// jsonIDs collects every string in a JSON document — keys and values, at
// any depth — that spells a UUID.
func jsonIDs(t *testing.T, raw string) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if id, ok := rowIDOf(k); ok {
					out = append(out, id)
				}
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case string:
			if id, ok := rowIDOf(x); ok {
				out = append(out, id)
			}
		}
	}
	walk(doc)
	return out
}

// referencesOf returns, by label, every reference the rows of revision revID
// hold — foreign-key columns, ref_id and targets, and each UUID inside the
// JSON documents — and the IDs of the rows the revision itself consists of.
func referencesOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, revID string) (refs map[string][]string, own map[string]bool) {
	t.Helper()
	const (
		ofRev    = `revision_id=$1::uuid`
		dimsOf   = `dimension_id IN (SELECT id FROM model.dimension_def WHERE revision_id=$1::uuid)`
		dashesOf = `dashboard_id IN (SELECT id FROM model.dashboard_def WHERE revision_id=$1::uuid)`
	)
	own = map[string]bool{}
	for _, id := range texts(t, ctx, pool, `
		          SELECT id::text FROM model.dimension_def WHERE `+ofRev+`
		UNION ALL SELECT id::text FROM model.dimension_member WHERE `+dimsOf+`
		UNION ALL SELECT id::text FROM model.metric_def WHERE `+ofRev+`
		UNION ALL SELECT id::text FROM model.grid_def WHERE `+ofRev+`
		UNION ALL SELECT id::text FROM model.form_def WHERE `+ofRev+`
		UNION ALL SELECT id::text FROM model.form_metric_mapping WHERE `+ofRev+`
		UNION ALL SELECT id::text FROM model.dashboard_folder WHERE `+ofRev+`
		UNION ALL SELECT id::text FROM model.dashboard_def WHERE `+ofRev+`
		UNION ALL SELECT id::text FROM model.dashboard_widget WHERE `+dashesOf+`
		UNION ALL SELECT id::text FROM model.integration_def WHERE `+ofRev+`
		UNION ALL SELECT id::text FROM workflow.workflow_def WHERE `+ofRev+`
		UNION ALL SELECT id::text FROM workflow.automation_rule WHERE `+ofRev, revID) {
		own[id] = true
	}
	refs = map[string][]string{}
	for _, c := range []struct {
		label, sql string
		json       bool
	}{
		{"dimension parent", `SELECT parent_dimension_id::text FROM model.dimension_def WHERE parent_dimension_id IS NOT NULL AND ` + ofRev, false},
		{"dimension source", `SELECT source_dimension_id::text FROM model.dimension_def WHERE source_dimension_id IS NOT NULL AND ` + ofRev, false},
		{"member parent", `SELECT parent_member_id::text FROM model.dimension_member WHERE parent_member_id IS NOT NULL AND ` + dimsOf, false},
		{"grid rollup source", `SELECT rollup_source_grid_id::text FROM model.grid_def WHERE rollup_source_grid_id IS NOT NULL AND ` + ofRev, false},
		{"mapping grid", `SELECT grid_id::text FROM model.form_metric_mapping WHERE grid_id IS NOT NULL AND ` + ofRev, false},
		{"mapping dimension keys", `SELECT dimension_mappings::text FROM model.form_metric_mapping WHERE ` + ofRev, true},
		{"form fields", `SELECT fields::text FROM model.form_def WHERE ` + ofRev, true},
		{"folder parent", `SELECT parent_id::text FROM model.dashboard_folder WHERE parent_id IS NOT NULL AND ` + ofRev, false},
		{"dashboard folder", `SELECT folder_id::text FROM model.dashboard_def WHERE folder_id IS NOT NULL AND ` + ofRev, false},
		{"widget ref_id", `SELECT ref_id FROM model.dashboard_widget WHERE ref_id IS NOT NULL AND ` + dashesOf, false},
		{"widget props", `SELECT widget_props::text FROM model.dashboard_widget WHERE widget_props IS NOT NULL AND ` + dashesOf, true},
		{"integration target", `SELECT target_id::text FROM model.integration_def WHERE target_id IS NOT NULL AND ` + ofRev, false},
		{"integration config", `SELECT config::text FROM model.integration_def WHERE ` + ofRev, true},
		{"workflow subject", `SELECT subject_config::text FROM workflow.workflow_def WHERE ` + ofRev, true},
		{"workflow context schema", `SELECT context_schema::text FROM workflow.workflow_def WHERE ` + ofRev, true},
		{"rule workflow", `SELECT workflow_def_id::text FROM workflow.automation_rule WHERE workflow_def_id IS NOT NULL AND ` + ofRev, false},
		{"rule form", `SELECT source_form_id::text FROM workflow.automation_rule WHERE source_form_id IS NOT NULL AND ` + ofRev, false},
		{"fact dimension keys", `SELECT dim_members::text FROM runtime.fact_input WHERE ` + ofRev, true},
	} {
		for _, v := range texts(t, ctx, pool, c.sql, revID) {
			if c.json {
				refs[c.label] = append(refs[c.label], jsonIDs(t, v)...)
			} else {
				refs[c.label] = append(refs[c.label], v)
			}
		}
	}
	return refs, own
}

// assertReferencesInside fails unless every kind of reference is present
// in revision revID and every one of them names a row of that revision.
func assertReferencesInside(t *testing.T, ctx context.Context, pool *pgxpool.Pool, revID, what string) {
	t.Helper()
	refs, own := referencesOf(t, ctx, pool, revID)
	for label, ids := range refs {
		if len(ids) == 0 {
			t.Errorf("%s: no %s reference at all — the fixture no longer covers it", what, label)
		}
		for _, id := range ids {
			if !own[id] {
				t.Errorf("%s: %s %s does not name a row of the imported revision", what, label, id)
			}
		}
	}
	var cfgTarget, colTarget string
	if err := pool.QueryRow(ctx, `
		SELECT config->>'target_id', target_id::text FROM model.integration_def
		WHERE type='rest_api' AND revision_id=$1::uuid`, revID).Scan(&cfgTarget, &colTarget); err != nil {
		t.Fatalf("%s: connector: %v", what, err)
	}
	var kind string
	if err := pool.QueryRow(ctx, `SELECT name FROM model.dimension_def WHERE id=$1::uuid AND revision_id=$2::uuid`, cfgTarget, revID).Scan(&kind); err != nil || kind != "City" || cfgTarget != colTarget {
		t.Errorf("%s: connector targets config=%s column=%s (%v), want both this revision's City dimension", what, cfgTarget, colTarget, err)
	}
}

// A model exported and imported into the database it came from is the case
// where every ID the package carries names an existing row — the source
// model's. Each reference must still land on the imported model's own rows,
// and the import must not be refused for carrying them.
func TestImportIntoSourceDatabaseKeepsReferencesInside(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)

	srcModel, srcRev, err := importInto(ctx, pool, app, user, decodePackage(t, referencePackage))
	if err != nil {
		t.Fatalf("import hand-written package: %v", err)
	}
	assertReferencesInside(t, ctx, pool, srcRev, "hand-written import")

	exported, err := CollectExport(ctx, pool, srcModel, srcRev, "Imported")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// Into the same tenant (a copy of the model) and into another one.
	otherApp, otherUser := tenantTarget(t, ctx, pool, "Other Co")
	for _, target := range []struct{ name, app, user string }{
		{"copy in the same tenant", app, user},
		{"another tenant", otherApp, otherUser},
	} {
		_, rev, err := importInto(ctx, pool, target.app, target.user, *exported)
		if err != nil {
			t.Fatalf("%s: import the export back: %v", target.name, err)
		}
		assertReferencesInside(t, ctx, pool, rev, target.name)
	}
}

// victimRows imports referencePackage for another tenant and returns the IDs
// of its rows by kind — the rows a crafted package would try to reach.
func victimRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	app, user := tenantTarget(t, ctx, pool, "Victim Co")
	_, rev, err := importInto(ctx, pool, app, user, decodePackage(t, referencePackage))
	if err != nil {
		t.Fatalf("import victim model: %v", err)
	}
	ids := map[string]string{"revision": rev}
	for kind, sql := range map[string]string{
		"dimension":   `SELECT id::text FROM model.dimension_def WHERE name='Region' AND revision_id=$1::uuid`,
		"member":      `SELECT m.id::text FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE m.code='N' AND d.revision_id=$1::uuid`,
		"metric":      `SELECT id::text FROM model.metric_def WHERE name='Revenue' AND revision_id=$1::uuid`,
		"grid":        `SELECT id::text FROM model.grid_def WHERE name='Plan' AND revision_id=$1::uuid`,
		"form":        `SELECT id::text FROM model.form_def WHERE name='request' AND revision_id=$1::uuid`,
		"folder":      `SELECT id::text FROM model.dashboard_folder WHERE name='Root' AND revision_id=$1::uuid`,
		"integration": `SELECT id::text FROM model.integration_def WHERE name='Feed' AND revision_id=$1::uuid`,
		"rule":        `SELECT id::text FROM workflow.automation_rule WHERE name='Run approval' AND revision_id=$1::uuid`,
	} {
		var id string
		if err := pool.QueryRow(ctx, sql, rev).Scan(&id); err != nil {
			t.Fatalf("victim %s: %v", kind, err)
		}
		ids[kind] = id
	}
	return ids
}

// importCount counts the models, and those named Crafted.
func importCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (models, crafted int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE name='Crafted') FROM core.model`).Scan(&models, &crafted); err != nil {
		t.Fatal(err)
	}
	return models, crafted
}

// A foreign-key column naming a row outside the package is refused, naming
// the field and the ID, and writes nothing — and the refusal reads the same
// whether that ID is another tenant's row or no row at all, so it tells the
// importer nothing about the other tenant.
func TestImportRefusesForeignKeysOutsideThePackage(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	victim := victimRows(t, ctx, pool)
	app, user := importTarget(t, ctx, pool)
	modelsBefore, _ := importCount(t, ctx, pool)
	str := func(s string) *string { return &s }

	for _, c := range []struct {
		name, field, kind string
		craft             func(p *Package, id string)
	}{
		{"dimension parent", "parent_dimension_id", "dimension", func(p *Package, id string) { p.Dimensions[2].ParentDimensionID = str(id) }},
		{"dimension source", "source_dimension_id", "dimension", func(p *Package, id string) { p.Dimensions[1].SourceDimensionID = str(id) }},
		{"member parent", "parent_member_id", "member", func(p *Package, id string) { p.Dimensions[0].Members[1].ParentMemberID = str(id) }},
		{"rate operand", "numerator", "metric", func(p *Package, id string) {
			p.Metrics = append(p.Metrics, Metric{ID: "met-margin", Name: "Margin", AggRule: "rate",
				AggNumeratorMetricID: str(id), AggDenominatorMetricID: str("met-rev")})
		}},
		{"grid rollup source", "rollup_source_grid_id", "grid", func(p *Package, id string) { p.Grids[1].RollupSourceGridID = str(id) }},
		{"mapping grid", "grid_id", "grid", func(p *Package, id string) { p.FormMappings[0].GridID = str(id) }},
		{"folder parent", "parent_id", "folder", func(p *Package, id string) { p.Folders[1].ParentID = str(id) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var errs []string
			for _, id := range []string{victim[c.kind], uuid.NewString()} {
				pkg := decodePackage(t, referencePackage)
				pkg.ModelName = "Crafted"
				c.craft(&pkg, id)
				_, _, err := importInto(ctx, pool, app, user, pkg)
				if err == nil {
					t.Fatalf("import accepted a %s naming %s", c.field, id)
				}
				if !strings.Contains(err.Error(), c.field) || !strings.Contains(err.Error(), id) ||
					!strings.Contains(err.Error(), "is not in the package") {
					t.Errorf("import error = %v, want it to name %s, the ID %s and that it is not in the package", err, c.field, id)
				}
				errs = append(errs, strings.ReplaceAll(err.Error(), id, "<id>"))
				if models, crafted := importCount(t, ctx, pool); models != modelsBefore || crafted != 0 {
					t.Errorf("refused import left rows behind: %d models (was %d), %d named Crafted", models, modelsBefore, crafted)
				}
			}
			if errs[0] != errs[1] {
				t.Errorf("the refusal differs for another tenant's row and for no row:\n  %s\n  %s", errs[0], errs[1])
			}
		})
	}
}

var uuidPattern = regexp.MustCompile(`(?i)[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}`)

// looseSites returns what revision revID holds in every place a reference
// without a foreign key is kept, one line per row, sorted.
func looseSites(t *testing.T, ctx context.Context, pool *pgxpool.Pool, revID string) []string {
	t.Helper()
	out := texts(t, ctx, pool, `
		          SELECT 'form '||name||' '||fields::text FROM model.form_def WHERE revision_id=$1::uuid
		UNION ALL SELECT 'mapping '||name||' '||dimension_mappings::text FROM model.form_metric_mapping WHERE revision_id=$1::uuid
		UNION ALL SELECT 'workflow '||name||' '||subject_config::text||' '||context_schema::text FROM workflow.workflow_def WHERE revision_id=$1::uuid
		UNION ALL SELECT 'integration '||name||' '||COALESCE(target_id::text,'-')||' '||config::text FROM model.integration_def WHERE revision_id=$1::uuid
		UNION ALL SELECT 'widget '||w.widget_type||' '||w.sort_order||' '||COALESCE(w.ref_id,'-')||' '||COALESCE(w.widget_props::text,'-')
		            FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id WHERE d.revision_id=$1::uuid
		UNION ALL SELECT 'fact '||value||' '||dim_members::text FROM runtime.fact_input WHERE revision_id=$1::uuid`, revID)
	sort.Strings(out)
	return out
}

// A reference held without a foreign key that names a row outside the
// package is dropped, not refused and not kept: the import succeeds, the
// other tenant's ID is nowhere in what it created, and what it created is
// the same whether that ID is another tenant's row or no row at all — so
// neither the outcome nor an error tells the importer the row exists. A
// dimension key of a cell becomes a fresh ID naming no row.
func TestImportDropsLooseReferencesOutsideThePackage(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	victim := victimRows(t, ctx, pool)
	app, user := importTarget(t, ctx, pool)

	str := func(s string) *string { return &s }
	widget := func(p *Package, typ string) *Widget {
		for i := range p.Dashboards[0].Widgets {
			if p.Dashboards[0].Widgets[i].WidgetType == typ {
				return &p.Dashboards[0].Widgets[i]
			}
		}
		t.Fatalf("no %s widget in the fixture", typ)
		return nil
	}
	integration := func(p *Package, name string) *Integration {
		for i := range p.Integrations {
			if p.Integrations[i].Name == name {
				return &p.Integrations[i]
			}
		}
		t.Fatalf("no integration %q in the fixture", name)
		return nil
	}
	raw := func(s, id string) json.RawMessage { return json.RawMessage(strings.ReplaceAll(s, "$ID", id)) }
	for _, c := range []struct {
		name, kind string
		craft      func(p *Package, id string)
	}{
		{"mapping dimension key", "dimension", func(p *Package, id string) {
			p.FormMappings[0].DimensionMappings = raw(`{"$ID": "region"}`, id)
		}},
		{"form field dimension", "dimension", func(p *Package, id string) {
			p.Forms[0].Fields = raw(`[{"name": "region", "type": "dimension", "dimension_id": "$ID"}]`, id)
		}},
		{"form field metric", "metric", func(p *Package, id string) {
			p.Forms[0].Fields = raw(`[{"name": "amount", "type": "number", "metric_id": "$ID"}]`, id)
		}},
		{"workflow subject form", "form", func(p *Package, id string) { p.Workflows[0].SubjectConfig = raw(`{"form_id": "$ID"}`, id) }},
		{"workflow subject grid", "grid", func(p *Package, id string) {
			p.Workflows[0].SubjectConfig = raw(`{"grid_id": "$ID", "metric_id": "met-rev"}`, id)
		}},
		{"workflow subject metric", "metric", func(p *Package, id string) {
			p.Workflows[0].SubjectConfig = raw(`{"grid_id": "grid-plan", "metric_id": "$ID"}`, id)
		}},
		{"workflow context schema", "dimension", func(p *Package, id string) {
			p.Workflows[0].ContextSchema = raw(`[{"key": "region", "type": "dimension_member", "dimension_id": "$ID"}]`, id)
		}},
		{"integration grid target", "grid", func(p *Package, id string) { integration(p, "Feed").TargetID = str(id) }},
		{"integration form target", "form", func(p *Package, id string) {
			ig := integration(p, "Feed")
			ig.TargetType, ig.TargetID = "form", str(id)
		}},
		{"connector dimension target", "dimension", func(p *Package, id string) { integration(p, "Cities").TargetID = str(id) }},
		{"connector config target", "dimension", func(p *Package, id string) {
			integration(p, "Cities").Config = raw(`{"kind": "rest_api/v1", "direction": "push", "target_type": "dimension", "target_id": "$ID"}`, id)
		}},
		{"grid widget", "grid", func(p *Package, id string) { widget(p, "grid").RefID = str(id) }},
		{"chart widget", "grid", func(p *Package, id string) { widget(p, "chart").RefID = str(id) }},
		{"import widget", "grid", func(p *Package, id string) { widget(p, "import").RefID = str(id) }},
		{"chart widget, ID spelled in upper case and braces", "grid", func(p *Package, id string) {
			widget(p, "chart").RefID = str("{" + strings.ToUpper(id) + "}")
		}},
		{"metric KPI widget", "metric", func(p *Package, id string) { widget(p, "metric_kpi").RefID = str(id) }},
		{"form widget", "form", func(p *Package, id string) { widget(p, "form").RefID = str(id) }},
		{"integration button", "integration", func(p *Package, id string) { widget(p, "integration_button").RefID = str(id) }},
		{"automation button", "rule", func(p *Package, id string) { widget(p, "automation_button").RefID = str(id) }},
		{"widget of a type this engine does not know", "grid", func(p *Package, id string) {
			p.Dashboards[0].Widgets = append(p.Dashboards[0].Widgets, Widget{WidgetType: "future_widget", RefID: str(id), SortOrder: 9})
		}},
		{"chart dimension", "dimension", func(p *Package, id string) {
			widget(p, "chart").Props = raw(`{"chart": {"dimension_id": "$ID", "metric_ids": ["met-rev"]}}`, id)
		}},
		{"chart series", "metric", func(p *Package, id string) {
			widget(p, "chart").Props = raw(`{"chart": {"dimension_id": "dim-region", "metric_ids": ["met-rev", "$ID"]}}`, id)
		}},
		{"scatter X metric", "metric", func(p *Package, id string) {
			widget(p, "chart").Props = raw(`{"chart": {"type": "scatter", "x_metric_id": "$ID", "y_metric_id": "met-rev"}}`, id)
		}},
		{"chart context default", "dimension", func(p *Package, id string) {
			widget(p, "chart").Props = raw(`{"chart": {"dimension_id": "dim-region", "context_defaults": {"$ID": "N"}}}`, id)
		}},
		{"KPI scope", "dimension", func(p *Package, id string) {
			widget(p, "metric_kpi").Props = raw(`{"kpi_scope": {"dimension_id": "$ID", "member_code": "N"}}`, id)
		}},
		{"grid layout axis", "dimension", func(p *Package, id string) {
			widget(p, "grid").Props = raw(`{"default_view": {"rows": ["$ID", "dim-region"], "cols": ["__metrics__"]}}`, id)
		}},
		{"grid layout filter", "dimension", func(p *Package, id string) {
			widget(p, "grid").Props = raw(`{"default_view": {"rows": ["dim-region"], "filter_sel": {"$ID": "N"}}}`, id)
		}},
		{"automation button's static target revision", "revision", func(p *Package, id string) {
			widget(p, "automation_button").Props = raw(`{"context": {"target_revision_id": "$ID", "region": "N"}}`, id)
		}},
		{"fact dimension key", "dimension", func(p *Package, id string) { p.Facts[0].DimMembers = raw(`{"$ID": "N"}`, id) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var shapes [2]string
			for i, id := range []string{victim[c.kind], uuid.NewString()} {
				pkg := decodePackage(t, referencePackage)
				c.craft(&pkg, id)
				_, rev, err := importInto(ctx, pool, app, user, pkg)
				if err != nil {
					t.Fatalf("import refused a package whose reference names %s outside it: %v", id, err)
				}
				sites := strings.Join(looseSites(t, ctx, pool, rev), "\n")
				if strings.Contains(strings.ToLower(sites), strings.ToLower(id)) {
					t.Errorf("the imported revision keeps %s:\n%s", id, sites)
				}
				refs, own := referencesOf(t, ctx, pool, rev)
				for label, ids := range refs {
					for _, ref := range ids {
						if !own[ref] && label != "fact dimension keys" && label != "mapping dimension keys" {
							t.Errorf("%s %s does not name a row of the imported revision", label, ref)
						}
					}
				}
				shapes[i] = uuidPattern.ReplaceAllString(sites, "<id>")
			}
			if shapes[0] != shapes[1] {
				t.Errorf("the import differs for another tenant's row and for no row:\n--- other tenant's row\n%s\n--- no row\n%s", shapes[0], shapes[1])
			}
		})
	}
}

// A reference held without a foreign key that names no row at all is what
// the engine itself leaves behind when that row is deleted (a form field or
// chart over a deleted dimension), so an ordinary export can carry one: it
// imports, dropped like any reference outside the package. A dimension key
// of a cell becomes one fresh ID — the same in every fact and mapping — so
// cells that differed only in it stay apart. A foreign-key column naming
// nothing is still refused.
func TestImportDropsReferencesToNothing(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	gone, gone2 := uuid.NewString(), uuid.NewString()

	pkg := decodePackage(t, referencePackage)
	pkg.Forms[0].Fields = json.RawMessage(`[{"name": "region", "type": "dimension", "dimension_id": "` + gone + `"}]`)
	pkg.Dashboards[0].Widgets[1].Props = json.RawMessage(`{"chart": {"dimension_id": "` + gone + `", "metric_ids": ["met-rev"]}}`)
	pkg.Dashboards[0].Widgets[0].RefID = &gone
	pkg.FormMappings[0].DimensionMappings = json.RawMessage(`{"` + gone + `": "region", "dim-region": "region"}`)
	pkg.Facts = []Fact{
		{MetricID: "met-rev", DimMembers: json.RawMessage(`{"` + gone + `": "A", "dim-region": "N"}`), Value: 5},
		{MetricID: "met-rev", DimMembers: json.RawMessage(`{"` + gone + `": "B", "dim-region": "N"}`), Value: 7},
		{MetricID: "met-rev", DimMembers: json.RawMessage(`{"` + gone2 + `": "A", "dim-region": "N"}`), Value: 9},
	}
	_, rev, err := importInto(ctx, pool, app, user, pkg)
	if err != nil {
		t.Fatalf("import with references to deleted rows: %v", err)
	}
	for what, sql := range map[string]string{
		"form field": `SELECT COALESCE(fields->0->>'dimension_id', 'dropped') FROM model.form_def WHERE revision_id=$1::uuid`,
		"chart":      `SELECT COALESCE(w.widget_props->'chart'->>'dimension_id', 'dropped') FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id WHERE w.widget_type='chart' AND d.revision_id=$1::uuid`,
		"grid ref":   `SELECT COALESCE(w.ref_id, 'dropped') FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id WHERE w.widget_type='grid' AND d.revision_id=$1::uuid`,
	} {
		if got := texts(t, ctx, pool, sql, rev); len(got) != 1 || got[0] != "dropped" {
			t.Errorf("%s = %v, want the reference to nothing dropped", what, got)
		}
	}

	// The facts: three cells still, each keyed by its fresh ID and the
	// imported Region; gone's fresh ID is shared with the mapping's key.
	region := texts(t, ctx, pool, `SELECT id::text FROM model.dimension_def WHERE name='Region' AND revision_id=$1::uuid`, rev)[0]
	fresh := map[string]map[string]bool{} // fresh key → the values it holds
	for _, dm := range texts(t, ctx, pool, `SELECT dim_members::text FROM runtime.fact_input WHERE revision_id=$1::uuid ORDER BY value`, rev) {
		var m map[string]string
		if err := json.Unmarshal([]byte(dm), &m); err != nil || len(m) != 2 || m[region] != "N" {
			t.Fatalf("fact dim_members = %s, want the imported Region and one fresh key", dm)
		}
		for k, v := range m {
			if k == region {
				continue
			}
			if k == gone || k == gone2 {
				t.Errorf("fact keeps the package's key %s", k)
			}
			if fresh[k] == nil {
				fresh[k] = map[string]bool{}
			}
			fresh[k][v] = true
		}
	}
	if len(fresh) != 2 {
		t.Fatalf("fresh keys = %v, want one for each of the two unresolved keys", fresh)
	}
	var goneKey string
	for k, vs := range fresh {
		if vs["B"] {
			goneKey = k
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM model.dimension_def WHERE id=$1::uuid`, k).Scan(&n); err != nil || n != 0 {
			t.Errorf("fresh key %s names a dimension (%d, %v)", k, n, err)
		}
	}
	if !fresh[goneKey]["A"] || !fresh[goneKey]["B"] {
		t.Errorf("the facts keyed by %s = %v, want A and B under one fresh key", gone, fresh)
	}
	var mapping string
	if err := pool.QueryRow(ctx, `SELECT dimension_mappings::text FROM model.form_metric_mapping WHERE revision_id=$1::uuid`, rev).Scan(&mapping); err != nil {
		t.Fatal(err)
	}
	if want := `"` + goneKey + `": "region"`; !strings.Contains(mapping, want) || !strings.Contains(mapping, `"`+region+`": "region"`) {
		t.Errorf("mapping dimension_mappings = %s, want %s beside the imported Region", mapping, want)
	}

	pkg = decodePackage(t, referencePackage)
	pkg.Grids[1].RollupSourceGridID = &gone
	if _, _, err := importInto(ctx, pool, app, user, pkg); err == nil ||
		!strings.Contains(err.Error(), "rollup_source_grid_id") || !strings.Contains(err.Error(), gone) {
		t.Errorf("import error = %v, want rollup_source_grid_id %s refused", err, gone)
	}
}

// Only reference positions are references. A string anywhere else is data
// and imports as written, even when it spells another tenant's row ID or a
// package ID: a member code, allowed members, a static context value, a
// connector's header, body and lookup table.
func TestImportLeavesDataThatSpellsAnID(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	victim := victimRows(t, ctx, pool)
	app, user := importTarget(t, ctx, pool)
	id := victim["grid"]

	pkg := decodePackage(t, referencePackage)
	pkg.Forms[0].Fields = json.RawMessage(`[{"name": "region", "type": "dimension", "dimension_id": "dim-region",
		"allowed_members": ["` + id + `", "dim-region"], "options": ["grid-plan"]}]`)
	for i := range pkg.Dashboards[0].Widgets {
		wd := &pkg.Dashboards[0].Widgets[i]
		switch wd.WidgetType {
		case "grid":
			wd.Props = json.RawMessage(`{"default_view": {"rows": ["dim-region"], "filter_sel": {"dim-region": "` + id + `"}}}`)
		case "automation_button":
			wd.Props = json.RawMessage(`{"context": {"region": "` + id + `", "note": "grid-plan"}}`)
		}
	}
	for i := range pkg.Integrations {
		if pkg.Integrations[i].Name == "Cities" {
			pkg.Integrations[i].Config = json.RawMessage(`{"kind": "rest_api/v1", "direction": "pull", "target_type": "dimension", "target_id": "dim-city",
				"request": {"method": "GET", "url": "https://api.example.test/cities",
				            "headers": [{"key": "X-App-Id", "value": "` + id + `", "enabled": true}, {"key": "X-Grid", "value": "grid-plan", "enabled": true}],
				            "body_mode": "json", "body_json": "{\"grid_id\": \"` + id + `\"}"},
				"mapping": {"fields": [{"source": "$.code", "target": "code", "target_kind": "member_code",
				            "transforms": [{"kind": "lookup", "lookup": {"` + id + `": "N"}}]}]}}`)
		}
	}
	pkg.Facts[0].DimMembers = json.RawMessage(`{"dim-region": "` + id + `"}`)
	_, rev, err := importInto(ctx, pool, app, user, pkg)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for what, c := range map[string]struct{ sql, want string }{
		"allowed member":                       {`SELECT fields->0->'allowed_members'->>0 FROM model.form_def WHERE revision_id=$1::uuid`, id},
		"allowed member equal to a package ID": {`SELECT fields->0->'allowed_members'->>1 FROM model.form_def WHERE revision_id=$1::uuid`, "dim-region"},
		"option equal to a package ID":         {`SELECT fields->0->'options'->>0 FROM model.form_def WHERE revision_id=$1::uuid`, "grid-plan"},
		"filter member code": {`SELECT w.widget_props->'default_view'->'filter_sel'->>(SELECT id::text FROM model.dimension_def WHERE name='Region' AND revision_id=$1::uuid)
			FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id WHERE w.widget_type='grid' AND d.revision_id=$1::uuid`, id},
		"static context value": {`SELECT w.widget_props->'context'->>'region' FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id
			WHERE w.widget_type='automation_button' AND d.revision_id=$1::uuid`, id},
		"static context value equal to a package ID": {`SELECT w.widget_props->'context'->>'note' FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id
			WHERE w.widget_type='automation_button' AND d.revision_id=$1::uuid`, "grid-plan"},
		"connector header":                       {`SELECT config->'request'->'headers'->0->>'value' FROM model.integration_def WHERE name='Cities' AND revision_id=$1::uuid`, id},
		"connector header equal to a package ID": {`SELECT config->'request'->'headers'->1->>'value' FROM model.integration_def WHERE name='Cities' AND revision_id=$1::uuid`, "grid-plan"},
		"connector body":                         {`SELECT config->'request'->>'body_json' FROM model.integration_def WHERE name='Cities' AND revision_id=$1::uuid`, `{"grid_id": "` + id + `"}`},
		"connector lookup table":                 {`SELECT config->'mapping'->'fields'->0->'transforms'->0->'lookup'->>'` + id + `' FROM model.integration_def WHERE name='Cities' AND revision_id=$1::uuid`, "N"},
		"fact member code": {`SELECT dim_members->>(SELECT id::text FROM model.dimension_def WHERE name='Region' AND revision_id=$1::uuid)
			FROM runtime.fact_input WHERE revision_id=$1::uuid`, id},
	} {
		if got := texts(t, ctx, pool, c.sql, rev); len(got) != 1 || got[0] != c.want {
			t.Errorf("%s = %v, want %q as written", what, got, c.want)
		}
	}
	var cfgTarget string
	if err := pool.QueryRow(ctx, `SELECT d.name FROM model.integration_def i JOIN model.dimension_def d ON d.id::text = i.config->>'target_id'
		WHERE i.name='Cities' AND i.revision_id=$1::uuid AND d.revision_id=$1::uuid`, rev).Scan(&cfgTarget); err != nil || cfgTarget != "City" {
		t.Errorf("connector config target = %q (%v), want the imported City", cfgTarget, err)
	}
}

// A revision's rows can name rows of the same model's other revisions: a
// workflow shared by every revision names those of the revision it was
// made in, and a copy the engine made before it remapped a field (a
// connector's config target, a grid widget's saved layout) names its
// source's. Its export points them at this revision's copy of each row —
// by lineage for a dimension, member or metric, by name otherwise — so the
// import keeps them, into the same database and into another tenant. A row
// of another model is never taken for a copy, even one with the same name.
func TestExportResolvesReferencesToOtherRevisions(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	model, rev, err := importInto(ctx, pool, app, user, decodePackage(t, referencePackage))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// Another model of the same application with a grid of the same name.
	_, otherRev, err := importInto(ctx, pool, app, user, decodePackage(t, referencePackage))
	if err != nil {
		t.Fatalf("import the other model: %v", err)
	}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// The sibling revision: copies of Region (and its member N), City, the
	// Plan grid and the request form, as a revision duplicate makes them.
	sib := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Sibling') RETURNING id::text`, model)
	copyDim := func(name string) string {
		return q(`INSERT INTO model.dimension_def (model_id, revision_id, name, lineage_id)
			SELECT model_id, $2::uuid, name, lineage_id FROM model.dimension_def WHERE revision_id=$1::uuid AND name=$3
			RETURNING id::text`, rev, sib, name)
	}
	sibRegion, sibCity := copyDim("Region"), copyDim("City")
	sibNorth := q(`INSERT INTO model.dimension_member (dimension_id, code, label, lineage_id)
		SELECT $2::uuid, m.code, m.label, m.lineage_id FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id
		WHERE d.revision_id=$1::uuid AND d.name='Region' AND m.code='N' RETURNING id::text`, rev, sibRegion)
	sibPlan := q(`INSERT INTO model.grid_def (model_id, revision_id, name) SELECT model_id, $2::uuid, name FROM model.grid_def
		WHERE revision_id=$1::uuid AND name='Plan' RETURNING id::text`, rev, sib)
	sibForm := q(`INSERT INTO model.form_def (model_id, revision_id, name, label) SELECT model_id, $2::uuid, name, label FROM model.form_def
		WHERE revision_id=$1::uuid AND name='request' RETURNING id::text`, rev, sib)
	otherPlan := q(`SELECT id::text FROM model.grid_def WHERE revision_id=$1::uuid AND name='Plan'`, otherRev)

	// This revision's rows naming the sibling's: a workflow shared by all
	// revisions, a connector (column and config), a grid widget's layout
	// and a member it names.
	exec(`INSERT INTO workflow.workflow_def (application_id, revision_id, name, trigger_event, subject_type, subject_config, context_schema, created_by, updated_by)
		VALUES ($1::uuid, NULL, 'Shared', 'manual', 'form', jsonb_build_object('form_id', $2::text),
		        jsonb_build_array(jsonb_build_object('key', 'region', 'type', 'dimension_member', 'dimension_id', $3::text)), $4::uuid, $4::uuid)`,
		app, sibForm, sibRegion, user)
	exec(`UPDATE model.integration_def SET target_id=$2::uuid, config=jsonb_set(config, '{target_id}', to_jsonb($2::text))
		WHERE revision_id=$1::uuid AND name='Cities'`, rev, sibCity)
	exec(`UPDATE model.dashboard_widget w SET ref_id=$2, widget_props=jsonb_build_object('default_view',
		  jsonb_build_object('rows', jsonb_build_array($3::text), 'cols', jsonb_build_array('__metrics__'), 'context', '[]'::jsonb,
		                     'filter_sel', jsonb_build_object($3::text, 'N')), 'member_id', $4::text)
		FROM model.dashboard_def d WHERE d.id=w.dashboard_id AND d.revision_id=$1::uuid AND w.widget_type='grid'`, rev, sibPlan, sibRegion, sibNorth)
	// Another model's grid of the same name is not a copy.
	exec(`UPDATE model.dashboard_widget w SET ref_id=$2 FROM model.dashboard_def d
		WHERE d.id=w.dashboard_id AND d.revision_id=$1::uuid AND w.widget_type='chart'`, rev, otherPlan)

	pkg, err := CollectExport(ctx, pool, model, rev, "Imported")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	exported, _ := json.Marshal(pkg)
	for what, id := range map[string]string{"Region": sibRegion, "City": sibCity, "member N": sibNorth, "Plan": sibPlan, "request": sibForm} {
		if strings.Contains(string(exported), id) {
			t.Errorf("the export still names the sibling revision's %s %s", what, id)
		}
	}
	if !strings.Contains(string(exported), otherPlan) {
		t.Errorf("the export took another model's grid %s for this revision's copy", otherPlan)
	}

	otherApp, otherUser := tenantTarget(t, ctx, pool, "Other Co")
	for _, target := range []struct{ name, app, user string }{
		{"copy in the same tenant", app, user},
		{"another tenant", otherApp, otherUser},
	} {
		_, newRev, err := importInto(ctx, pool, target.app, target.user, *pkg)
		if err != nil {
			t.Fatalf("%s: import: %v", target.name, err)
		}
		assertReferencesInside(t, ctx, pool, newRev, target.name)
		for what, c := range map[string]struct{ sql, want string }{
			"shared workflow's form": {`SELECT f.name FROM workflow.workflow_def w JOIN model.form_def f ON f.id::text = w.subject_config->>'form_id'
				WHERE w.name='Shared' AND w.revision_id=$1::uuid AND f.revision_id=$1::uuid`, "request"},
			"shared workflow's dimension": {`SELECT d.name FROM workflow.workflow_def w JOIN model.dimension_def d ON d.id::text = w.context_schema->0->>'dimension_id'
				WHERE w.name='Shared' AND w.revision_id=$1::uuid AND d.revision_id=$1::uuid`, "Region"},
			"grid widget": {`SELECT g.name FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id JOIN model.grid_def g ON g.id::text = w.ref_id
				WHERE w.widget_type='grid' AND d.revision_id=$1::uuid AND g.revision_id=$1::uuid`, "Plan"},
			"grid layout": {`SELECT dd.name FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id
				JOIN model.dimension_def dd ON dd.id::text = w.widget_props->'default_view'->'rows'->>0
				WHERE w.widget_type='grid' AND d.revision_id=$1::uuid AND dd.revision_id=$1::uuid AND w.widget_props->'default_view'->'filter_sel' ? dd.id::text`, "Region"},
			"chart widget on another model's grid": {`SELECT COALESCE(w.ref_id, 'dropped') FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id
				WHERE w.widget_type='chart' AND d.revision_id=$1::uuid`, "dropped"},
		} {
			if got := texts(t, ctx, pool, c.sql, newRev); len(got) != 1 || got[0] != c.want {
				t.Errorf("%s: %s = %v, want %s", target.name, what, got, c.want)
			}
		}
	}
}

// rewriteRefs resolves the reference positions and nothing else, drops what
// it is told to drop, and leaves an untouched document byte for byte.
func TestRewriteRefsPositions(t *testing.T) {
	ids := map[string]string{"d1": "D1", "m1": "M1", "g1": "G1"}
	fn := func(kind, id string) (string, bool) {
		if n, ok := ids[id]; ok {
			return n + ":" + kind, true
		}
		return id, id != "gone"
	}
	for _, c := range []struct {
		name string
		spec jsonRefs
		in   string
		want string
	}{
		{"fields", jsonRefs{}, `[{"dimension_id":"d1","metric_id":"m1","allowed_members":["d1"],"options":["g1"]}]`,
			`[{"allowed_members":["d1"],"dimension_id":"D1:dimension","metric_id":"M1:metric","options":["g1"]}]`},
		{"dropped field", jsonRefs{}, `{"form_id":"gone","grid_id":"g1"}`, `{"grid_id":"G1:grid"}`},
		{"widget props", jsonRefs{}, `{"chart":{"x_metric_id":"m1","metric_ids":["m1","gone"],"context_defaults":{"d1":"d1","gone":"N"}},` +
			`"default_view":{"rows":["__metrics__","d1","gone"],"cols":[],"context":["d1"],"filter_sel":{"d1":"g1"}},"context":{"target_revision_id":"gone","region":"d1"}}`,
			`{"chart":{"context_defaults":{"D1:dimension":"d1"},"metric_ids":["M1:metric"],"x_metric_id":"M1:metric"},"context":{"region":"d1"},` +
				`"default_view":{"cols":[],"context":["D1:dimension"],"filter_sel":{"D1:dimension":"g1"},"rows":["__metrics__","D1:dimension"]}}`},
		{"cell keys", jsonRefs{keys: true}, `{"d1":"g1","gone":"N"}`, `{"D1:dimension":"g1"}`},
		{"config", jsonRefs{topOnly: true}, `{"target_id":"g1","request":{"headers":[{"key":"k","value":"g1"}],"x_id":"g1"},"auth":{"key_id":"g1"}}`,
			`{"auth":{"key_id":"g1"},"request":{"headers":[{"key":"k","value":"g1"}],"x_id":"g1"},"target_id":"G1:target"}`},
		{"untouched", jsonRefs{}, "{ \"note\" : \"d1\",\n \"n\": 1.50 }", "{ \"note\" : \"d1\",\n \"n\": 1.50 }"},
		{"not JSON", jsonRefs{}, `{"dimension_id":`, `{"dimension_id":`},
	} {
		got, _ := rewriteRefs(json.RawMessage(c.in), c.spec, fn)
		if string(got) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

func TestRowIDOf(t *testing.T) {
	const canon = "0f8fad5b-d9cb-469f-a165-70867728950e"
	for _, s := range []string{
		canon, strings.ToUpper(canon), "{" + canon + "}", "urn:uuid:" + canon,
		strings.ReplaceAll(canon, "-", ""), "0f8fad5bd9cb-469fa165-70867728950e",
	} {
		if got, ok := rowIDOf(s); !ok || got != canon {
			t.Errorf("rowIDOf(%q) = %q, %v; want %q", s, got, ok, canon)
		}
	}
	for _, s := range []string{"", "dim-region", "__metrics__", canon + "0", "https://x.test/" + canon, "N"} {
		if got, ok := rowIDOf(s); ok {
			t.Errorf("rowIDOf(%q) = %q, want not an ID", s, got)
		}
	}
}
