package gateway

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// TestDimensionParentSameRevision: the developer's dimension create and
// PATCH check a parent dimension with the rules the AI Developer shares
// (metricformula.ValidateParentDimension): a parent of the same revision is
// accepted; another revision's is refused with INVALID_PARENT_DIMENSION, as
// are a dimension's own id, a hierarchy cycle and a time dimension.
func TestDimensionParentSameRevision(t *testing.T) {
	f := setupDimFormulaFixture(t)
	dims := "/api/developer/dimensions"
	code := metricformula.CodeInvalidParentDimension

	city := f.call("POST", dims, map[string]any{"name": "city", "revision_id": f.revID, "parent_dimension_id": f.region})
	var parent string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(parent_dimension_id::text,'') FROM model.dimension_def WHERE id=$1::uuid`, city).Scan(&parent); err != nil || parent != f.region {
		t.Fatalf("city's parent %q (err %v), want region", parent, err)
	}

	otherRev := f.call("POST", "/api/developer/revisions", map[string]any{"name": "Other"})
	var otherRegion string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='region'`, otherRev).Scan(&otherRegion); err != nil {
		t.Fatalf("the copied revision's region: %v", err)
	}
	f.refused("a parent in another revision (POST)", "POST", dims,
		map[string]any{"name": "town", "revision_id": f.revID, "parent_dimension_id": otherRegion}, code)
	f.refused("a parent in another revision (PATCH)", "PATCH", dims+"/"+city,
		map[string]any{"parent_dimension_id": otherRegion}, code)
	f.refused("its own parent", "PATCH", dims+"/"+city, map[string]any{"parent_dimension_id": city}, code)
	f.refused("a hierarchy cycle", "PATCH", dims+"/"+f.region, map[string]any{"parent_dimension_id": city}, code)
	f.refused("a parent that does not exist", "PATCH", dims+"/"+city,
		map[string]any{"parent_dimension_id": "00000000-0000-0000-0000-000000000000"}, code)
	month := f.call("POST", dims, map[string]any{"name": "month", "revision_id": f.revID,
		"dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1})
	f.refused("a time dimension's parent (PATCH)", "PATCH", dims+"/"+month, map[string]any{"parent_dimension_id": f.region}, code)
	f.refused("a time dimension's parent (POST)", "POST", dims, map[string]any{"name": "week", "revision_id": f.revID,
		"dimension_type": "time", "time_granularity": "week", "fiscal_year_start_month": 1, "parent_dimension_id": f.region}, code)

	// Detaching still works.
	f.call("PATCH", dims+"/"+city, map[string]any{"parent_dimension_id": nil})
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(parent_dimension_id::text,'') FROM model.dimension_def WHERE id=$1::uuid`, city).Scan(&parent); err != nil || parent != "" {
		t.Errorf("city's parent %q after detach (err %v), want none", parent, err)
	}
}
