package modeltransfer

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A dimension's total rule and a metric's own rules by dimension (migration
// 174) travel with a model: an import stores them, by the imported
// dimensions, and an export gives them back by the package's ids.
func TestImportCarriesTotalRules(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)

	pkg := formulaPackage(nil, nil)
	pkg.Version = PackageVersion
	pkg.Dimensions = append(pkg.Dimensions, Dimension{ID: "dim-scen", Name: "scenario", AggRule: "first",
		Members: []Member{{ID: "mem-base", Code: "BASE", Label: "Base"}, {ID: "mem-down", Code: "DOWN", Label: "Downside"}}})
	pkg.Grids[0].Dimensions = append(pkg.Grids[0].Dimensions, GridDimension{DimensionID: "dim-scen"})
	pkg.Metrics[0].DimensionRules = map[string]string{"dim-scen": "sum"}
	var modelID, revID string
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		modelID, revID, err = Import(ctx, tx, ImportRequest{ApplicationID: app, Package: pkg}, user)
		return err
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	var dimRule, metricRule string
	if err := pool.QueryRow(ctx, `
		SELECT d.agg_rule, r.rule FROM model.dimension_def d
		JOIN model.metric_dimension_rule r ON r.dimension_id = d.id
		JOIN model.metric_def m ON m.id = r.metric_id AND m.name = 'rev'
		WHERE d.revision_id = $1::uuid AND d.name = 'scenario'`, revID).Scan(&dimRule, &metricRule); err != nil {
		t.Fatalf("read the imported rules: %v", err)
	}
	if dimRule != "first" || metricRule != "sum" {
		t.Errorf("imported rules: dimension %q, metric %q; want first, sum", dimRule, metricRule)
	}

	out, err := CollectExport(ctx, pool, modelID, revID, "")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	scenID := ""
	for _, d := range out.Dimensions {
		if d.Name == "scenario" {
			scenID = d.ID
			if d.AggRule != "first" {
				t.Errorf("exported dimension rule %q", d.AggRule)
			}
		}
	}
	for _, m := range out.Metrics {
		if m.Name == "rev" && (len(m.DimensionRules) != 1 || m.DimensionRules[scenID] != "sum") {
			t.Errorf("exported metric rules %v, want {%s: sum}", m.DimensionRules, scenID)
		}
	}

	// A rule a time dimension cannot have is refused.
	bad := formulaPackage(nil, nil)
	bad.ModelName = "Bad rules"
	bad.Version = PackageVersion
	bad.Metrics[0].DimensionRules = map[string]string{"dim-month": "first"}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, _, err := Import(ctx, tx, ImportRequest{ApplicationID: app, Package: bad}, user)
		return err
	})
	if err == nil {
		t.Error("a total rule over a time dimension imported")
	}
}
