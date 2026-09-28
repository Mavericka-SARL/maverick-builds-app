package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// TestCreatePropertyGroupingDimension is the AI Developer's parity with the
// developer console's property grouping: create_dimension takes
// source_dimension_id (id or name) + source_property + derive_members,
// validated by the same metricformula.ValidateGrouping (same code), and a
// revision copy (create_revision) points the copy's grouping at the copy's
// own source dimension.
func TestCreatePropertyGroupingDimension(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()
	run := func(tool string, params map[string]any) (string, string, error) {
		t.Helper()
		return exec.Execute(ctx, tool, mustJSON(t, params))
	}

	empID := ""
	if _, id, err := run("create_dimension", map[string]any{
		"name": "employees",
		"members": []map[string]any{
			{"code": "E1", "label": "E1", "properties": map[string]string{"area": "North"}},
			{"code": "E2", "label": "E2", "properties": map[string]string{"area": "South"}},
			{"code": "E3", "label": "E3", "properties": map[string]string{"area": "North"}},
		},
	}); err != nil {
		t.Fatal(err)
	} else {
		empID = id
	}
	if _, _, err := run("add_dimension_property", map[string]any{"dimension_id": "employees", "name": "area"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run("create_dimension", map[string]any{"name": "month", "dimension_type": "time",
		"time_granularity": "month", "fiscal_year_start_month": 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run("create_dimension", map[string]any{"name": "dept"}); err != nil {
		t.Fatal(err)
	}

	// The developer endpoint's refusals, with its code.
	for _, tc := range []struct {
		what   string
		params map[string]any
	}{
		{"an undeclared property", map[string]any{"source_dimension_id": "employees", "source_property": "zone"}},
		{"a property without a source", map[string]any{"source_property": "area"}},
		{"a source without a property", map[string]any{"source_dimension_name": "employees"}},
		{"a time source", map[string]any{"source_dimension_id": "month", "source_property": "area"}},
		{"a time grouping", map[string]any{"source_dimension_id": "employees", "source_property": "area",
			"dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1}},
		{"a parent dimension and a grouping", map[string]any{"source_dimension_id": "employees", "source_property": "area",
			"parent_dimension_name": "dept"}},
		{"an unknown source", map[string]any{"source_dimension_id": "nobody", "source_property": "area"}},
		{"derive_members without a grouping", map[string]any{"derive_members": true}},
	} {
		params := map[string]any{"name": "bad"}
		for k, v := range tc.params {
			params[k] = v
		}
		if _, _, err := run("create_dimension", params); err == nil || !strings.Contains(err.Error(), metricformula.CodeInvalidGrouping) {
			t.Errorf("%s: err %v, want %s", tc.what, err, metricformula.CodeInvalidGrouping)
		}
	}

	// By name, the property in another case: stored as declared, members
	// derived from the values.
	res, areaID, err := run("create_dimension", map[string]any{"name": "area", "source_dimension_id": "employees",
		"source_property": "AREA", "derive_members": true})
	if err != nil {
		t.Fatalf("create area: %v", err)
	}
	if !strings.Contains(res, "derived 2 member(s)") {
		t.Errorf("result %q should say 2 members were derived", res)
	}
	var source, prop, codes string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(d.source_dimension_id::text,''), COALESCE(d.source_property,''),
		       (SELECT string_agg(code, ',' ORDER BY code) FROM model.dimension_member WHERE dimension_id=d.id)
		FROM model.dimension_def d WHERE d.id=$1::uuid`, areaID).Scan(&source, &prop, &codes); err != nil {
		t.Fatal(err)
	}
	if source != empID || prop != "area" || codes != "North,South" {
		t.Errorf("area: source %s property %q members %q; want %s, area, North,South", source, prop, codes, empID)
	}

	// list_dimensions says what it groups.
	listing, err := aiassistant.NewToolExecutor(pool, modelID, revID).Execute(ctx, "list_dimensions", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing, "Dimension: area (groups employees by its property area") {
		t.Errorf("list_dimensions should describe the grouping; got:\n%s", listing)
	}

	// A copy's grouping points at the copy's employees.
	_, revB, err := run("create_revision", map[string]any{"name": "B", "source_revision_id": revID})
	if err != nil {
		t.Fatal(err)
	}
	var copiedSource, copiedEmp string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(a.source_dimension_id::text,''), e.id::text
		FROM model.dimension_def a JOIN model.dimension_def e ON e.revision_id=a.revision_id AND e.name='employees'
		WHERE a.revision_id=$1::uuid AND a.name='area'`, revB).Scan(&copiedSource, &copiedEmp); err != nil {
		t.Fatal(err)
	}
	if copiedSource != copiedEmp {
		t.Errorf("copied area's source %s, want the copy's employees %s", copiedSource, copiedEmp)
	}
}
