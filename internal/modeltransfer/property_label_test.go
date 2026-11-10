package modeltransfer

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A dimension property's label (migration 175) travels with a model.
func TestImportCarriesPropertyLabels(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)

	pkg := formulaPackage(nil, nil)
	pkg.Version = PackageVersion
	pkg.Dimensions[0].TypedProperties = []DimProperty{{Name: "sales_segment", DataType: "text", Label: "Sales Segment"}, {Name: "tier", DataType: "text"}}
	var modelID, revID string
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		modelID, revID, err = Import(ctx, tx, ImportRequest{ApplicationID: app, Package: pkg}, user)
		return err
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	out, err := CollectExport(ctx, pool, modelID, revID, "")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	got := map[string]string{}
	for _, d := range out.Dimensions {
		for _, p := range d.TypedProperties {
			got[p.Name] = p.Label
		}
	}
	if got["sales_segment"] != "Sales Segment" || got["tier"] != "" {
		t.Errorf("exported labels %v, want sales_segment \"Sales Segment\" and none for tier", got)
	}
}
