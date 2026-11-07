package recordpost_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// A count posting counts the records whose field holds anything: counting a
// month's records by a dimension or text field (a period, an account code)
// used to post nothing, every record skipped as "not a number", and each
// import check read MISSING though the files were loaded.
func TestCountPostingCountsAnyFilledField(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE model.form_metric_mapping SET aggregation='count', source_field='region' WHERE id=$1::uuid`, f.mapping); err != nil {
		t.Fatal(err)
	}
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	rows := spendRows("10", "20", "5") // EU, US, EU
	if _, err := ps.Import(ctx, recordpost.ImportRequest{Form: f.form, CreatorID: f.userID,
		Header: []string{"region", "amount"}, Rows: rows, Inline: true}); err != nil {
		t.Fatal(err)
	}
	if eu, us := f.spend(t, "EU"), f.spend(t, "US"); eu != 2 || us != 1 {
		t.Errorf("records counted by region: EU = %v, US = %v; want 2 and 1", eu, us)
	}
}

// A posting may map only its target metric's grid dimensions: values kept
// per member of another dimension showed in the grid but read 0 in every
// formula.
func TestMappingRefusesADimensionTheTargetGridLacks(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	var revID, siteDim string
	if err := f.pool.QueryRow(ctx, `SELECT revision_id::text FROM model.form_def WHERE id=$1::uuid`, f.form.ID).Scan(&revID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type)
		VALUES ($1::uuid,$2::uuid,'Sites','standard') RETURNING id::text`, f.form.ModelID, revID).Scan(&siteDim); err != nil {
		t.Fatal(err)
	}
	refs := crudapp.MappingRefs{FormID: f.form.ID, TargetMetricID: f.metricID, SourceField: "amount",
		DimensionMappings: map[string]string{f.regionDim: "region"}}
	if err := crudapp.CheckMappingRefs(ctx, f.pool, f.form.ModelID, revID, refs); err != nil {
		t.Fatalf("the grid's own dimension: %v", err)
	}
	refs.DimensionMappings = map[string]string{}
	err := crudapp.CheckMappingRefs(ctx, f.pool, f.form.ModelID, revID, refs)
	if !crudapp.IsMappingRefError(err) || !strings.Contains(err.Error(), "leaves region unmapped") {
		t.Fatalf("a grid dimension left unmapped: %v", err)
	}
	refs.DimensionMappings = map[string]string{f.regionDim: "region", siteDim: "code"}
	err = crudapp.CheckMappingRefs(ctx, f.pool, f.form.ModelID, revID, refs)
	if !crudapp.IsMappingRefError(err) || !strings.Contains(err.Error(), "Sites, which spend's grid does not have") {
		t.Fatalf("a dimension the grid lacks: %v", err)
	}
}

// A grid keeps a dimension a posting into one of its metrics maps: removed,
// the posting would keep storing values per member of it that no formula
// reads.
func TestGridKeepsADimensionAPostingMaps(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	var gridID string
	if err := f.pool.QueryRow(ctx, `SELECT grid_id::text FROM model.grid_metric WHERE metric_id=$1::uuid`, f.metricID).Scan(&gridID); err != nil {
		t.Fatal(err)
	}
	msg, err := crudapp.GridDimensionPostedRefusal(ctx, f.pool, gridID, f.regionDim)
	if err != nil || !strings.Contains(msg, `"spend" (into spend) still map it`) {
		t.Fatalf("removal while the posting maps region: %q %v", msg, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE model.form_metric_mapping SET dimension_mappings='{}' WHERE id=$1::uuid`, f.mapping); err != nil {
		t.Fatal(err)
	}
	if msg, err := crudapp.GridDimensionPostedRefusal(ctx, f.pool, gridID, f.regionDim); err != nil || msg != "" {
		t.Errorf("once the posting no longer maps it: %q %v", msg, err)
	}
	// A child dimension of region (stores within a region) posting into the
	// grid keeps region too: without it, nothing would read the stores' values.
	var revID, storeDim string
	if err := f.pool.QueryRow(ctx, `SELECT revision_id::text FROM model.form_def WHERE id=$1::uuid`, f.form.ID).Scan(&revID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, parent_dimension_id)
		VALUES ($1::uuid,$2::uuid,'Stores','standard',$3::uuid) RETURNING id::text`, f.form.ModelID, revID, f.regionDim).Scan(&storeDim); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE model.form_metric_mapping SET dimension_mappings=jsonb_build_object($2::text,'region') WHERE id=$1::uuid`, f.mapping, storeDim); err != nil {
		t.Fatal(err)
	}
	if msg, _ := crudapp.GridDimensionPostedRefusal(ctx, f.pool, gridID, f.regionDim); !strings.Contains(msg, "still map it") {
		t.Errorf("removal while a posting maps a child dimension of region: %q", msg)
	}
}

// The values a grid's metrics keep along a dimension go with it (owner
// decision, 2026-11-07): kept, the grid summed them while formulas read 0.
func TestRemovingAGridDimensionRemovesItsValues(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	var gridID, revID string
	if err := f.pool.QueryRow(ctx, `SELECT gm.grid_id::text, md.revision_id::text FROM model.grid_metric gm
		JOIN model.metric_def md ON md.id = gm.metric_id WHERE gm.metric_id=$1::uuid`, f.metricID).Scan(&gridID, &revID); err != nil {
		t.Fatal(err)
	}
	for _, region := range []string{"EU", "US"} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
			VALUES ($1::uuid,$2::uuid,$3::uuid,jsonb_build_object($4::text,$5::text),7)`,
			f.form.ModelID, revID, f.metricID, f.regionDim, region); err != nil {
			t.Fatal(err)
		}
	}
	other := func() float64 {
		return f.count(t, `SELECT count(*) FROM runtime.fact_input WHERE metric_id=$1::uuid`, f.metricID)
	}
	// Not on the grid: nothing goes.
	if _, err := f.pool.Exec(ctx, crudapp.RemoveGridDimensionSQL, gridID, f.metricID); err != nil || other() != 2 {
		t.Fatalf("a dimension the grid does not have: %v, %v values left, want 2", err, other())
	}
	if _, err := f.pool.Exec(ctx, crudapp.RemoveGridDimensionSQL, gridID, f.regionDim); err != nil {
		t.Fatal(err)
	}
	if n := other(); n != 0 {
		t.Errorf("values kept along region after its removal: %v, want 0", n)
	}
	if n := f.count(t, `SELECT count(*) FROM model.grid_dimension WHERE grid_id=$1::uuid`, gridID); n != 0 {
		t.Errorf("grid dimensions after the removal: %v, want 0", n)
	}
}
