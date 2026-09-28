package gateway

import (
	"testing"
)

// TestAIDeveloperChangesAGrouping is the AI Developer's parity with the
// developer console's dimension PATCH, through propose -> confirm: the
// assistant creates "grp" grouping employees by their area property, then
// update_dimension switches it to the region property (deriving the new
// members) and renames it, and after promotion a grid by the grouping
// totals salary by region, not by area.
func TestAIDeveloperChangesAGrouping(t *testing.T) {
	var steps []map[string]any
	add := func(tool, description string, params map[string]any) int {
		steps = append(steps, proposeStep(tool, description, params))
		return len(steps)
	}
	employees := add("create_dimension", "Create dimension 'employees'", map[string]any{
		"name": "employees",
		"members": []map[string]any{
			{"code": "E1", "label": "E1", "properties": map[string]string{"area": "North", "region": "EMEA"}},
			{"code": "E2", "label": "E2", "properties": map[string]string{"area": "North", "region": "APAC"}},
			{"code": "E3", "label": "E3", "properties": map[string]string{"area": "South", "region": "EMEA"}},
		},
	})
	add("add_dimension_property", "Declare 'area'", map[string]any{"dimension_id": ref(employees), "name": "area"})
	add("add_dimension_property", "Declare 'region'", map[string]any{"dimension_id": ref(employees), "name": "region"})
	add("create_dimension", "Create 'grp' grouping employees by area", map[string]any{
		"name": "grp", "source_dimension_id": "employees", "source_property": "area", "derive_members": true,
	})
	add("update_dimension", "Group by region instead, derive its members, rename to 'zone'", map[string]any{
		"dimension_id": "grp", "name": "zone", "source_property": "region", "derive_members": true,
	})
	salary := add("create_metric", "Input metric 'salary'",
		map[string]any{"name": "salary", "is_input": true, "agg_rule": "sum", "format": "number"})
	staff := add("create_grid", "Create the 'Staff' grid", map[string]any{"name": "Staff"})
	add("add_grid_dimension", "Put employees on Staff", map[string]any{"grid_id": ref(staff), "dimension_id": ref(employees)})
	add("add_grid_metric", "Add salary", map[string]any{"grid_id": ref(staff), "metric_id": ref(salary)})
	zoneSal := add("create_metric", "Calculated 'zone_sal' = salary", map[string]any{
		"name": "zone_sal", "is_input": false, "formula": "salary", "agg_rule": "sum", "format": "number",
	})
	byZone := add("create_grid", "Create the 'By zone' grid", map[string]any{"name": "By zone"})
	add("add_grid_dimension", "Put zone on it", map[string]any{"grid_id": ref(byZone), "dimension_id": "zone"})
	add("add_grid_metric", "Add zone_sal", map[string]any{"grid_id": ref(byZone), "metric_id": ref(zoneSal)})

	b := runAIBuild(t, "group employees by area, then switch the grouping to region and call it zone", steps)
	modelID, do := b.modelID, b.do

	stored := b.q(`SELECT source_property || ':' || COALESCE((SELECT string_agg(code, ',' ORDER BY code)
		FROM model.dimension_member WHERE dimension_id=d.id), '')
		FROM model.dimension_def d WHERE d.revision_id=$1::uuid AND d.name='zone'`, b.draft)
	if stored != "region:APAC,EMEA,North,South" {
		t.Errorf("draft zone = %q, want region with the area members kept and the region members derived", stored)
	}

	active := b.promote()
	empID := b.q(`SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='employees'`, modelID, active)
	salaryID := b.q(`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='salary'`, modelID, active)
	gridID := b.q(`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='By zone'`, modelID, active)
	for code, v := range map[string]float64{"E1": 100, "E2": 200, "E3": 50} {
		if status, body := do("POST", "/api/cells", map[string]any{
			"model_id": modelID, "metric_id": salaryID, "revision_id": active,
			"dim_codes": map[string]string{empID: code}, "value": v,
		}); status < 200 || status >= 300 {
			t.Fatalf("write salary at %s: status %d\n%s", code, status, body)
		}
	}
	// By region: EMEA = E1 + E3, APAC = E2; the area members group nobody.
	gridValuesByName(t, func(path string) (int, []byte) { return do("GET", path, nil) }, gridID, map[string]float64{
		cellKey("zone_sal", "EMEA"): 150, cellKey("zone_sal", "APAC"): 200,
	})
}
