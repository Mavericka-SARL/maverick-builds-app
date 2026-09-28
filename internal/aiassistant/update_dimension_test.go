package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// TestUpdateDimension is the AI Developer's twin of the developer console's
// PATCH /api/developer/dimensions/{id}: every field partially, the property
// grouping set / changed / re-derived / cleared, the shared validation codes
// (INVALID_GROUPING, INVALID_PARENT_DIMENSION, DIMENSION_IN_USE,
// DIMENSION_NAME_TAKEN), and another model's or revision's dimension out of
// reach.
func TestUpdateDimension(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()
	run := func(tool string, params map[string]any) (string, string, error) {
		t.Helper()
		return exec.Execute(ctx, tool, mustJSON(t, params))
	}
	mustRun := func(tool string, params map[string]any) string {
		t.Helper()
		_, id, err := run(tool, params)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return id
	}
	type dimRow struct {
		name, agg         string
		parent, src, prop *string
		members           []string
	}
	load := func(id string) dimRow {
		t.Helper()
		var r dimRow
		if err := pool.QueryRow(ctx, `SELECT name, agg_rule, parent_dimension_id::text, source_dimension_id::text, source_property
			FROM model.dimension_def WHERE id=$1::uuid`, id).Scan(&r.name, &r.agg, &r.parent, &r.src, &r.prop); err != nil {
			t.Fatal(err)
		}
		rows, err := pool.Query(ctx, `SELECT code FROM model.dimension_member WHERE dimension_id=$1::uuid ORDER BY code`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var c string
			_ = rows.Scan(&c)
			r.members = append(r.members, c)
		}
		return r
	}
	wantCode := func(what string, err error, code string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), code) {
			t.Errorf("%s: err %v, want %s", what, err, code)
		}
	}

	empID := mustRun("create_dimension", map[string]any{
		"name": "employees",
		"members": []map[string]any{
			{"code": "E1", "label": "E1", "properties": map[string]string{"area": "North", "region": "EMEA"}},
			{"code": "E2", "label": "E2", "properties": map[string]string{"area": "South", "region": "APAC"}},
		},
	})
	mustRun("add_dimension_property", map[string]any{"dimension_id": "employees", "name": "area"})
	mustRun("add_dimension_property", map[string]any{"dimension_id": "employees", "name": "region"})
	deptID := mustRun("create_dimension", map[string]any{"name": "dept"})
	mustRun("create_dimension", map[string]any{"name": "month", "dimension_type": "time",
		"time_granularity": "month", "fiscal_year_start_month": 1})
	groupID := mustRun("create_dimension", map[string]any{"name": "grp"})

	// Name, rollup rule: partial; the rest keeps its value.
	if _, _, err := run("update_dimension", map[string]any{"dimension_id": "grp", "name": "grouping"}); err != nil {
		t.Fatal(err)
	}
	if r := load(groupID); r.name != "grouping" || r.agg != "sum" {
		t.Errorf("after rename: %+v", r)
	}
	if _, _, err := run("update_dimension", map[string]any{"dimension_id": groupID, "agg_rule": "average"}); err != nil {
		t.Fatal(err)
	}
	if r := load(groupID); r.name != "grouping" || r.agg != "average" {
		t.Errorf("after agg_rule: %+v", r)
	}
	_, _, err := run("update_dimension", map[string]any{"dimension_id": groupID, "name": "dept"})
	wantCode("a taken name", err, metricformula.CodeDimensionNameTaken)
	_, _, err = run("update_dimension", map[string]any{"dimension_id": groupID})
	if err == nil || !strings.Contains(err.Error(), "nothing to change") {
		t.Errorf("an empty update: err %v", err)
	}

	// Parent dimension: set by name, refused onto itself / a cycle / a time
	// dimension, detached by null.
	if _, _, err := run("update_dimension", map[string]any{"dimension_id": groupID, "parent_dimension_name": "dept"}); err != nil {
		t.Fatal(err)
	}
	if r := load(groupID); r.parent == nil || *r.parent != deptID {
		t.Errorf("parent not set: %+v", r)
	}
	_, _, err = run("update_dimension", map[string]any{"dimension_id": "dept", "parent_dimension_id": groupID})
	wantCode("a hierarchy cycle", err, metricformula.CodeInvalidParentDimension)
	_, _, err = run("update_dimension", map[string]any{"dimension_id": "dept", "parent_dimension_id": "dept"})
	wantCode("its own parent", err, metricformula.CodeInvalidParentDimension)
	_, _, err = run("update_dimension", map[string]any{"dimension_id": "month", "parent_dimension_id": "dept"})
	wantCode("a time dimension's parent", err, metricformula.CodeInvalidParentDimension)
	// Parent and grouping are exclusive.
	_, _, err = run("update_dimension", map[string]any{"dimension_id": groupID, "source_dimension_id": "employees", "source_property": "area"})
	wantCode("a grouping with a parent", err, metricformula.CodeInvalidGrouping)
	if _, _, err := run("update_dimension", map[string]any{"dimension_id": groupID, "parent_dimension_id": nil}); err != nil {
		t.Fatal(err)
	}
	if r := load(groupID); r.parent != nil {
		t.Errorf("parent not detached: %+v", r)
	}

	// Grouping: the shared validator's refusals.
	for _, tc := range []struct {
		what   string
		params map[string]any
	}{
		{"an undeclared property", map[string]any{"source_dimension_id": "employees", "source_property": "zone"}},
		{"a property without a source", map[string]any{"source_property": "area"}},
		{"a time source", map[string]any{"source_dimension_name": "month", "source_property": "area"}},
		{"its own members", map[string]any{"source_dimension_id": groupID, "source_property": "area"}},
		{"derive without a grouping", map[string]any{"derive_members": true}},
	} {
		tc.params["dimension_id"] = groupID
		_, _, err := run("update_dimension", tc.params)
		wantCode(tc.what, err, metricformula.CodeInvalidGrouping)
	}

	// Set (derive), change the property (derive the new values), clear.
	res, _, err := run("update_dimension", map[string]any{"dimension_id": groupID, "source_dimension_name": "employees",
		"source_property": "AREA", "derive_members": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "derived 2 member(s)") {
		t.Errorf("set result: %q", res)
	}
	if r := load(groupID); r.src == nil || *r.src != empID || r.prop == nil || *r.prop != "area" ||
		strings.Join(r.members, ",") != "North,South" {
		t.Errorf("after set: %+v (the declared spelling is stored)", r)
	}
	if _, _, err := run("update_dimension", map[string]any{"dimension_id": groupID, "source_property": "region",
		"derive_members": true}); err != nil {
		t.Fatal(err)
	}
	if r := load(groupID); r.src == nil || *r.src != empID || r.prop == nil || *r.prop != "region" ||
		strings.Join(r.members, ",") != "APAC,EMEA,North,South" {
		t.Errorf("after property change: %+v", r)
	}
	// A new value appears; derive alone adds it.
	mustRun("add_dimension_member", map[string]any{"dimension_id": "employees", "code": "E3", "label": "E3",
		"properties": map[string]string{"region": "LATAM"}})
	if _, _, err := run("update_dimension", map[string]any{"dimension_id": groupID, "derive_members": true}); err != nil {
		t.Fatal(err)
	}
	if r := load(groupID); !strings.Contains(strings.Join(r.members, ","), "LATAM") {
		t.Errorf("re-derive: %+v", r)
	}
	// Clearing the source while a formula names the dimension: DIMENSION_IN_USE.
	mustRun("create_metric", map[string]any{"name": "headcount", "is_input": true, "format": "number"})
	mustRun("create_metric", map[string]any{"name": "emea_heads", "formula": `SUMIFS(headcount, grouping, "EMEA")`, "format": "number"})
	_, _, err = run("update_dimension", map[string]any{"dimension_id": groupID, "source_dimension_id": nil})
	wantCode("clearing a source a formula reads through", err, metricformula.CodeDimensionInUse)
	if err != nil && !strings.HasPrefix(err.Error(), metricformula.CodeDimensionInUse+": ") {
		t.Errorf("clearing a source a formula reads through: %q does not start with its code", err)
	}
	// Changing only the property keeps the relation: allowed.
	if _, _, err := run("update_dimension", map[string]any{"dimension_id": groupID, "source_property": "area"}); err != nil {
		t.Errorf("a property change under a formula: %v", err)
	}
	mustRun("delete_metric", map[string]any{"metric_id": "emea_heads"})
	if _, _, err := run("update_dimension", map[string]any{"dimension_id": groupID, "source_dimension_id": nil}); err != nil {
		t.Fatal(err)
	}
	if r := load(groupID); r.src != nil || r.prop != nil {
		t.Errorf("after clear: %+v", r)
	}

	// Another model's dimension, as the target, the parent or the source.
	otherModel := seedModel(t, pool)
	otherRev := seedRevision(t, pool, otherModel, "Other")
	_, otherDim, err := aiassistant.NewWriteExecutor(pool, otherModel, otherRev).Execute(ctx, "create_dimension",
		mustJSON(t, map[string]any{"name": "elsewhere"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, params := range []map[string]any{
		{"dimension_id": otherDim, "name": "stolen"},
		{"dimension_id": groupID, "parent_dimension_id": otherDim},
		{"dimension_id": groupID, "source_dimension_id": otherDim, "source_property": "x"},
	} {
		if _, _, err := run("update_dimension", params); err == nil || !strings.Contains(err.Error(), "different model") {
			t.Errorf("another model's dimension %v: err %v", params, err)
		}
	}
	var otherName string
	_ = pool.QueryRow(ctx, `SELECT name FROM model.dimension_def WHERE id=$1::uuid`, otherDim).Scan(&otherName)
	if otherName != "elsewhere" {
		t.Errorf("another model's dimension was renamed to %q", otherName)
	}

	// Another revision's dimension with no counterpart here: refused as the
	// target and as the parent. A revision-less (legacy) dimension of the
	// same model passes the model check but not the revision rule.
	revB := seedRevision(t, pool, modelID, "Rev B")
	_, orphan, err := aiassistant.NewWriteExecutor(pool, modelID, revB).Execute(ctx, "create_dimension",
		mustJSON(t, map[string]any{"name": "only_in_b"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, params := range []map[string]any{
		{"dimension_id": orphan, "name": "x"},
		{"dimension_id": groupID, "parent_dimension_id": orphan},
	} {
		if _, _, err := run("update_dimension", params); err == nil || !strings.Contains(err.Error(), "different revision") {
			t.Errorf("another revision's dimension %v: err %v", params, err)
		}
	}
	var legacy string
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, name, agg_rule) VALUES ($1::uuid, 'legacy', 'sum')
		RETURNING id::text`, modelID).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	_, _, err = run("update_dimension", map[string]any{"dimension_id": groupID, "parent_dimension_id": legacy})
	wantCode("a revision-less parent", err, metricformula.CodeInvalidParentDimension)
	_, _, err = run("create_dimension", map[string]any{"name": "child", "parent_dimension_id": legacy})
	wantCode("create_dimension with a revision-less parent", err, metricformula.CodeInvalidParentDimension)
	// create_dimension resolves the parent's name within the working
	// revision: a same-named dimension of another revision is not it.
	_, _, err = run("create_dimension", map[string]any{"name": "child", "parent_dimension_name": "only_in_b"})
	wantCode("create_dimension with another revision's parent name", err, metricformula.CodeInvalidParentDimension)
}
