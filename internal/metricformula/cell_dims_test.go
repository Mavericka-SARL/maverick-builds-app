package metricformula

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// A formula reads a dimension at the cell — bare, as dim.property, through
// PARENT — only where the cell has a member of it: on its own grid's
// dimensions. Anywhere else the read is blank at every cell, a related
// dimension (a property grouping of a grid dimension) included, so it is
// refused at save (once the metric is placed), at placement, when the grid
// loses the dimension and at activation (DIMENSION_NOT_ON_METRIC).
func TestValidateCellDimensionsOnTheMetricsGrid(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid, 'tier', 'text')`, f.product); err != nil {
		t.Fatal(err)
	}

	// selfy is placed on G1 [region, month].
	for _, tc := range []struct {
		formula string
		refuse  bool
		mention string
	}{
		{`IF(product = "P1", 1, 0)`, true, "product"},
		{`IF(segment = "SMB", 1, 0)`, true, "region.segment"}, // a grouping of region: read it through region
		{`IF(PARENT(product) = "", 1, 0)`, true, "PARENT(product)"},
		{`IF(product.tier = "A", 1, 0)`, true, "product.tier"},
		{`LOOKUP(revenue, region, IF(product = "P1", "EMEA", "US"))`, true, "product"}, // a member argument is read at the cell
		{`IF(region = "EMEA", 1, 0)`, false, ""},
		{`IF(REGION = "EMEA", 1, 0)`, false, ""},
		{`IF(month = "2026-01", 1, 0)`, false, ""},
		{`IF(PARENT(region) = "World", 1, 0)`, false, ""},
		{`revenue * region.factor`, false, ""},
		{`SUMIFS(revenue, region.segment, "SMB")`, false, ""}, // a criteria range is not a cell read
		{`COUNTIFS(product, "P*")`, false, ""},
		{`channel * 2`, false, ""}, // a metric of the same name as a dimension
	} {
		_, err := f.validate(t, f.selfy, "selfy", tc.formula)
		if !tc.refuse {
			if err != nil {
				t.Errorf("%s: want saved, got %v", tc.formula, err)
			}
			continue
		}
		if codeOf(err) != formula.CodeDimensionNotOnMetric {
			t.Errorf("%s: want %s, got %v", tc.formula, formula.CodeDimensionNotOnMetric, err)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "selfy") || !strings.Contains(msg, tc.mention) {
			t.Errorf("%s: the message must name the metric and %q: %s", tc.formula, tc.mention, msg)
		}
	}

	// A new metric's dimensions are not known yet: it saves, and placing it
	// on a grid without the dimension is refused.
	if _, err := f.validate(t, "", "pflag", `IF(product = "P1", 1, 0)`); err != nil {
		t.Fatalf("an unplaced metric saves: %v", err)
	}
	var pflag string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula)
		VALUES ($1::uuid, $2::uuid, 'pflag', false, $3) RETURNING id::text`, f.modelID, f.revID, `IF(product = "P1", 1, 0)`).Scan(&pflag); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTime(ctx, f.pool, f.modelID, f.revID); err != nil {
		t.Fatalf("unplaced, activation has nothing to say: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, f.g1, pflag); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGridTime(ctx, f.pool, f.modelID, f.revID, f.g1); codeOf(err) != formula.CodeDimensionNotOnMetric {
		t.Errorf("placed on G1 [region, month]: want %s, got %v", formula.CodeDimensionNotOnMetric, err)
	}
	// Activation names the metric and how to fix it.
	err := ValidateTime(ctx, f.pool, f.modelID, f.revID)
	if codeOf(err) != formula.CodeDimensionNotOnMetric || !strings.Contains(err.Error(), "pflag") || !strings.Contains(err.Error(), "put product on the grid") {
		t.Errorf("activation: want %s naming pflag and the fix, got %v", formula.CodeDimensionNotOnMetric, err)
	}
	// A grid that carries the dimension is fine: G2 [product].
	if _, err := f.pool.Exec(ctx, `UPDATE model.grid_metric SET grid_id=$1::uuid WHERE metric_id=$2::uuid`, f.g2, pflag); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGridTime(ctx, f.pool, f.modelID, f.revID, f.g2); err != nil {
		t.Errorf("placed on G2 [product]: %v", err)
	}

	// Removing a dimension from a grid whose metric reads it at the cell.
	var rflag string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula)
		VALUES ($1::uuid, $2::uuid, 'rflag', false, $3) RETURNING id::text`, f.modelID, f.revID, `IF(region = "EMEA", 1, 0)`).Scan(&rflag); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, f.g1, rflag); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGridDimensional(ctx, f.pool, f.modelID, f.revID, f.g1); err != nil {
		t.Fatalf("region still on G1: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM model.grid_dimension WHERE grid_id=$1::uuid AND dimension_id=$2::uuid`, f.g1, f.region); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGridDimensional(ctx, f.pool, f.modelID, f.revID, f.g1); codeOf(err) != formula.CodeDimensionNotOnMetric || !strings.Contains(err.Error(), "rflag") {
		t.Errorf("region removed from G1: want %s for rflag, got %v", formula.CodeDimensionNotOnMetric, err)
	}
}
