package modeltransfer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A package's references to rows it does not hold — a typo in a
// hand-edited package — are left out by the import, and each one is named
// with where it was: the import says what it did not link instead of
// succeeding with less than the package said. A package without any names
// nothing.
func TestImportReportsDroppedReferences(t *testing.T) {
	clean := decodePackage(t, referencePackage)
	if notes := clean.DroppedReferences(); len(notes) != 0 {
		t.Errorf("the reference package, whose every reference resolves, has notes %q", notes)
	}

	str := func(s string) *string { return &s }
	pkg := decodePackage(t, referencePackage)
	pkg.Grids[0].Metrics = append(pkg.Grids[0].Metrics, GridMetric{MetricID: "met-revv"})
	pkg.Grids[0].Dimensions = append(pkg.Grids[0].Dimensions, GridDimension{DimensionID: "dim-typo"})
	pkg.Dashboards[0].FolderID = str("fold-typo")
	pkg.AutomationRules[0].WorkflowDefID = str("wf-typo")
	pkg.AutomationRules[0].SourceIntegrationID = str("int-typo")
	pkg.Forms[0].KeepsDimensionID = "dim-keeps-typo"
	pkg.Forms[0].Fields = json.RawMessage(strings.Replace(string(pkg.Forms[0].Fields), `"dim-region"`, `"dim-field-typo"`, 1))
	pkg.Dashboards[0].Widgets[1].Props = json.RawMessage(strings.Replace(string(pkg.Dashboards[0].Widgets[1].Props), `"met-cost"`, `"met-chart-typo"`, 1))
	pkg.FormRecords = append(pkg.FormRecords, FormRecord{FormID: "form-typo", Data: json.RawMessage(`{}`)})
	typoMapping := pkg.FormMappings[0]
	typoMapping.ID, typoMapping.Name, typoMapping.TargetMetricID = "map-typo", "Typo", "met-map-typo"
	pkg.FormMappings = append(pkg.FormMappings, typoMapping)
	pkg.Facts = append(pkg.Facts,
		Fact{MetricID: "met-gone", DimMembers: json.RawMessage(`{"dim-region": "N"}`), Value: 1},
		Fact{MetricID: "met-gone", DimMembers: json.RawMessage(`{"dim-region": "N"}`), Value: 2},
		Fact{MetricID: "met-rev", DimMembers: json.RawMessage(`{"dim-region": "N"}`), Value: 3, SourceMappingID: str("map-typo")},
		Fact{MetricID: "met-rev", DimMembers: json.RawMessage(`{"dim-region": "N"}`), Value: 4, SourceMappingID: str("map-nowhere")},
		Fact{MetricID: "met-rev", DimMembers: json.RawMessage(`{"dim-fact-typo": "N"}`), Value: 5})

	notes := pkg.DroppedReferences()
	all := strings.Join(notes, "\n")
	for _, want := range []string{
		`grid "Plan": metric "met-revv" is not in the package`,
		`grid "Plan": dimension "dim-typo" is not in the package`,
		`dashboard "Overview": folder "fold-typo" is not in the package`,
		`automation rule "Run approval": workflow "wf-typo" is not in the package`,
		`automation rule "Run approval": integration "int-typo" is not in the package`,
		`table "request": dimension "dim-keeps-typo" is not in the package`,
		`table "request": dimension "dim-field-typo" is not in the package`,
		`dashboard "Overview", widget "chart": metric "met-chart-typo" is not in the package`,
		`1 record of table "form-typo", which is not in the package`,
		`table mapping "Typo": metric "met-map-typo" is not in the package`,
		`2 values of metric "met-gone", which is not in the package`,
		`1 value posted by table mapping "Typo"`,
		`1 value posted by table mapping "map-nowhere", which is not in the package`,
		`1 value names dimension "dim-fact-typo", which is not in the package`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("notes do not say %q; they are:\n%s", want, all)
		}
	}
	if len(notes) != 14 {
		t.Errorf("%d notes, want 14 (one per dropped link):\n%s", len(notes), all)
	}

	// Naming them changes nothing: the package imports as before.
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	if _, _, err := importInto(ctx, pool, app, user, pkg); err != nil {
		t.Fatalf("import: %v", err)
	}
}

// A member whose parent names it back (or itself) is refused, as every
// member edit refuses a hierarchy cycle; it imported before, and every
// total over that dimension walked the loop.
func TestImportRefusesMemberParentCycle(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	str := func(s string) *string { return &s }
	pkg := Package{ModelName: "Loop", Dimensions: []Dimension{{ID: "dim-loop", Name: "Loop", Members: []Member{
		{ID: "mem-a", Code: "A", Label: "A", ParentMemberID: str("mem-b")},
		{ID: "mem-b", Code: "B", Label: "B", ParentMemberID: str("mem-a")},
		{ID: "mem-c", Code: "C", Label: "C", ParentMemberID: str("mem-a")},
	}}}}
	_, _, err := importInto(ctx, pool, app, user, pkg)
	if err == nil || !strings.Contains(err.Error(), `of "Loop" is its own ancestor`) {
		t.Errorf("import error = %v, want the cycle in Loop refused", err)
	}
	pkg.Dimensions[0].Members = []Member{{ID: "mem-a", Code: "A", Label: "A", ParentMemberID: str("mem-a")}}
	_, _, err = importInto(ctx, pool, app, user, pkg)
	if err == nil || !strings.Contains(err.Error(), `member "A" of "Loop" is its own ancestor`) {
		t.Errorf("import error = %v, want a member that is its own parent refused", err)
	}
}

// A member's parent belongs to the member's own dimension or to the
// dimension's declared parent dimension, as every member edit requires.
// A package's member whose parent is in any other dimension is imported
// without the parent (owner's decision, 2026-11-09), its values kept, and
// the import names the member and the parent. That is what a model whose
// developer cleared a dimension's parent dimension exports, so it is not
// refused.
func TestImportMemberParentInUnrelatedDimension(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	str := func(s string) *string { return &s }
	pkg := decodePackage(t, referencePackage)
	pkg.Dimensions = append(pkg.Dimensions, Dimension{ID: "dim-product", Name: "Product", Members: []Member{
		{ID: "mem-p1", Code: "P1", Label: "Product 1", ParentMemberID: str("mem-all")}}})
	// City declares Region its parent dimension: Oslo under North is kept,
	// and so is a member under another city (an engine export carries one
	// when the parent dimension was declared after the members existed).
	pkg.Dimensions[2].Members[0].ParentMemberID = str("mem-north")
	pkg.Dimensions[2].Members = append(pkg.Dimensions[2].Members, Member{ID: "mem-centre", Code: "OSL-C", Label: "Oslo centre", ParentMemberID: str("mem-oslo")})
	pkg.Metrics = append(pkg.Metrics, Metric{ID: "met-units", Name: "Units", IsInput: true})
	pkg.Grids = append(pkg.Grids, Grid{ID: "grid-units", Name: "Units", Metrics: []GridMetric{{MetricID: "met-units"}}, Dimensions: []GridDimension{{DimensionID: "dim-product"}}})
	pkg.Facts = append(pkg.Facts, Fact{MetricID: "met-units", DimMembers: json.RawMessage(`{"dim-product": "P1"}`), Value: 7})

	notes := pkg.DroppedReferences()
	want := `dimension "Product", member "P1": its parent "ALL" is a member of "Region", which is neither "Product" nor its parent dimension, so the member is imported without a parent`
	if len(notes) != 1 || notes[0] != want {
		t.Errorf("notes = %q, want only %q", notes, want)
	}

	_, revID, err := importInto(ctx, pool, app, user, pkg)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	got := texts(t, ctx, pool, `
		SELECT d.name || ':' || m.code || '<' || COALESCE(p.code, '-')
		FROM model.dimension_member m
		JOIN model.dimension_def d ON d.id = m.dimension_id
		LEFT JOIN model.dimension_member p ON p.id = m.parent_member_id
		WHERE d.revision_id = $1::uuid AND d.name IN ('Product', 'City')
		ORDER BY 1`, revID)
	if strings.Join(got, " ") != "City:OSL-C<OSL City:OSL<N Product:P1<-" {
		t.Errorf("members = %q, want P1 without a parent, Oslo under North and its centre under Oslo", got)
	}
	values := texts(t, ctx, pool, `
		SELECT fi.value::text FROM runtime.fact_input fi JOIN model.metric_def m ON m.id = fi.metric_id
		WHERE m.revision_id = $1::uuid AND m.name = 'Units'`, revID)
	if strings.Join(values, ",") != "7" {
		t.Errorf("P1's values = %q, want them kept (7)", values)
	}
}

// A rule scoped to one integration keeps its scope when the package gives
// the rule no ID of its own (a hand-written package): the scope was set by
// the rule's ID, so such a rule arrived firing after every integration of
// the application, and no note said so.
func TestImportScopesRuleWithoutID(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	pkg := decodePackage(t, `{
	  "model_name": "Scope",
	  "metrics": [{"id": "met-rev", "name": "Revenue", "is_input": true}],
	  "grids": [{"id": "grid-plan", "name": "Plan", "metrics": [{"metric_id": "met-rev"}], "dimensions": []}],
	  "workflows": [{"id": "wf-close", "name": "Close", "trigger_event": "manual"}],
	  "integrations": [{"id": "int-feed", "name": "Feed", "target_type": "grid", "target_id": "grid-plan", "config": {}}],
	  "automation_rules": [{"name": "After the feed", "trigger_type": "integration_completed", "workflow_name": "Close",
	    "workflow_def_id": "wf-close", "source_integration_id": "int-feed"}]
	}`)
	_, revID, err := importInto(ctx, pool, app, user, pkg)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	got := texts(t, ctx, pool, `
		SELECT r.name || ':' || COALESCE(i.name, 'every integration')
		FROM workflow.automation_rule r LEFT JOIN model.integration_def i ON i.id = r.source_integration_id
		WHERE r.revision_id = $1::uuid`, revID)
	if strings.Join(got, ",") != "After the feed:Feed" {
		t.Errorf("rules = %q, want After the feed scoped to Feed", got)
	}
}
