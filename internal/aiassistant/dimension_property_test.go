package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// TestAddDimensionProperty covers the AI's property declaration tool: it is
// checked by the developer endpoint's validator (same codes), writes into
// the executor's revision, and — with member property values set by
// create_dimension and add_dimension_member — lets a formula read
// dimension.property, typed by the declaration.
func TestAddDimensionProperty(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	// create_dimension's members carry properties; nothing is declared yet,
	// so the result says so.
	res, dimID, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
		"name": "region",
		"members": []map[string]any{
			{"code": "EMEA", "label": "EMEA", "properties": map[string]string{"segment": "SMB", "factor": "2"}},
		},
	}))
	if err != nil {
		t.Fatalf("create region: %v", err)
	}
	if !strings.Contains(res, "factor, segment not declared") {
		t.Errorf("create_dimension result should name the undeclared properties; got %q", res)
	}

	// Declared by NAME (the model routinely passes names), defaulting to text.
	if _, _, err := exec.Execute(ctx, "add_dimension_property", mustJSON(t, map[string]any{
		"dimension_id": "region", "name": "segment",
	})); err != nil {
		t.Fatalf("declare segment: %v", err)
	}
	if _, _, err := exec.Execute(ctx, "add_dimension_property", mustJSON(t, map[string]any{
		"dimension_id": dimID, "name": " factor ", "data_type": "number",
	})); err != nil {
		t.Fatalf("declare factor: %v", err)
	}

	// The developer endpoint's refusals, with its codes.
	for _, tc := range []struct {
		what   string
		params map[string]any
		code   string
	}{
		{"a name with a space", map[string]any{"dimension_id": dimID, "name": "Sales Segment", "data_type": "text"}, metricformula.CodeInvalidPropertyName},
		{"a name starting with a digit", map[string]any{"dimension_id": dimID, "name": "2nd", "data_type": "text"}, metricformula.CodeInvalidPropertyName},
		{"an unknown type", map[string]any{"dimension_id": dimID, "name": "tier", "data_type": "money"}, metricformula.CodeInvalidPropertyType},
		{"a name differing only in case", map[string]any{"dimension_id": dimID, "name": "SEGMENT", "data_type": "text"}, metricformula.CodePropertyNameTaken},
	} {
		if _, _, err := exec.Execute(ctx, "add_dimension_property", mustJSON(t, tc.params)); err == nil || !strings.Contains(err.Error(), tc.code) {
			t.Errorf("%s: err %v, want %s", tc.what, err, tc.code)
		}
	}
	// A dimension of another model is refused like every other AI write.
	otherModel := seedModel(t, pool)
	otherRev := seedRevision(t, pool, otherModel, "Other")
	_, otherDim, err := aiassistant.NewWriteExecutor(pool, otherModel, otherRev).Execute(ctx, "create_dimension",
		mustJSON(t, map[string]any{"name": "elsewhere"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec.Execute(ctx, "add_dimension_property", mustJSON(t, map[string]any{
		"dimension_id": otherDim, "name": "x", "data_type": "text",
	})); err == nil {
		t.Error("declaring a property on another model's dimension must be refused")
	}

	var declared []string
	rows, err := pool.Query(ctx, `
		SELECT p.name || ':' || p.data_type FROM model.dimension_property p
		JOIN model.dimension_def d ON d.id = p.dimension_id
		WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid ORDER BY p.name`, modelID, revID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		declared = append(declared, s)
	}
	rows.Close()
	if strings.Join(declared, ",") != "factor:number,segment:text" {
		t.Errorf("declared in the revision: %v, want factor:number, segment:text", declared)
	}

	// add_dimension_member stores its properties; a declared key draws no note.
	res, _, err = exec.Execute(ctx, "add_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": dimID, "code": "US", "label": "US", "properties": map[string]string{"segment": "ENT", "factor": "3"},
	}))
	if err != nil {
		t.Fatalf("add US: %v", err)
	}
	if strings.Contains(res, "not declared") {
		t.Errorf("declared properties drew a note: %q", res)
	}
	var props string
	if err := pool.QueryRow(ctx, `SELECT properties::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='US'`, dimID).Scan(&props); err != nil {
		t.Fatal(err)
	}
	if props != `{"factor": "3", "segment": "ENT"}` {
		t.Errorf("US properties stored as %s", props)
	}

	// list_dimensions shows the typed declarations and the values.
	listing, err := aiassistant.NewToolExecutor(pool, modelID, revID).Execute(ctx, "list_dimensions", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Declared properties: factor (number), segment (text)", "US (US) {factor=3, segment=ENT}"} {
		if !strings.Contains(listing, want) {
			t.Errorf("list_dimensions lacks %q:\n%s", want, listing)
		}
	}

	// A formula reading the declared property saves; an undeclared one is
	// refused with the developer path's code.
	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "revenue", "is_input": true, "format": "number",
	})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "scaled", "formula": "revenue * region.factor", "format": "number",
	})); err != nil {
		t.Errorf("a formula reading a declared property: %v", err)
	}
	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "bad", "formula": "revenue * region.colour", "format": "number",
	})); err == nil || !strings.Contains(err.Error(), "UNKNOWN_PROPERTY") {
		t.Errorf("an undeclared property: err %v, want UNKNOWN_PROPERTY", err)
	}
}

// TestUpdateAndDeleteDimensionProperty covers the AI's property rename,
// retype and delete tools against the developer PATCH/DELETE they mirror:
// the same validator codes, a rename that moves every member's value (keys
// matched case-insensitively) and a grouped dimension's source_property, a
// retype that keeps the name, a delete after which a formula naming the
// property is refused, and no reach into another dimension or model.
func TestUpdateAndDeleteDimensionProperty(t *testing.T) {
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
			t.Fatalf("%s %v: %v", tool, params, err)
		}
		return id
	}

	dimID := mustRun("create_dimension", map[string]any{
		"name": "region",
		"members": []map[string]any{
			{"code": "EMEA", "label": "EMEA", "properties": map[string]string{"fact": "2", "seg": "1"}},
			{"code": "US", "label": "US", "properties": map[string]string{"FACT": "3"}},
		},
	})
	for _, name := range []string{"fact", "seg", "tier"} {
		mustRun("add_dimension_property", map[string]any{"dimension_id": dimID, "name": name})
	}
	segID := ""
	if err := pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_property WHERE dimension_id=$1::uuid AND name='seg'`, dimID).Scan(&segID); err != nil {
		t.Fatal(err)
	}
	// A dimension grouped by the property follows the rename.
	groupedID := mustRun("create_dimension", map[string]any{"name": "grouped"})
	if _, err := pool.Exec(ctx, `UPDATE model.dimension_def SET source_dimension_id=$1::uuid, source_property='fact' WHERE id=$2::uuid`, dimID, groupedID); err != nil {
		t.Fatal(err)
	}

	// The developer PATCH's refusals, with its codes; nothing to change and
	// an undeclared property are refused too.
	for _, tc := range []struct {
		what   string
		params map[string]any
		want   string
	}{
		{"a name taken regardless of case", map[string]any{"dimension_id": dimID, "property": "fact", "name": "SEG"}, metricformula.CodePropertyNameTaken},
		{"a name with a space", map[string]any{"dimension_id": dimID, "property": "fact", "name": "the factor"}, metricformula.CodeInvalidPropertyName},
		{"an unknown type", map[string]any{"dimension_id": dimID, "property": "fact", "data_type": "money"}, metricformula.CodeInvalidPropertyType},
		{"nothing to change", map[string]any{"dimension_id": dimID, "property": "fact"}, "nothing to change"},
		{"an undeclared property", map[string]any{"dimension_id": dimID, "property": "colour", "name": "color"}, "not declared"},
		{"no property", map[string]any{"dimension_id": dimID, "name": "x"}, "property is required"},
	} {
		if _, _, err := run("update_dimension_property", tc.params); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", tc.what, err, tc.want)
		}
	}

	// Rename and retype in one step, named by dimension NAME and the
	// property's current name in another case.
	res, _, err := run("update_dimension_property", map[string]any{
		"dimension_id": "region", "property": "FACT", "name": "factor", "data_type": "number",
	})
	if err != nil {
		t.Fatalf("rename fact: %v", err)
	}
	if !strings.Contains(res, "renamed to 'factor'") || !strings.Contains(res, "type text → number") {
		t.Errorf("rename result %q", res)
	}
	for code, want := range map[string]string{"EMEA": `{"seg": "1", "factor": "2"}`, "US": `{"factor": "3"}`} {
		var props string
		if err := pool.QueryRow(ctx, `SELECT properties::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, dimID, code).Scan(&props); err != nil {
			t.Fatal(err)
		}
		if props != want {
			t.Errorf("%s properties after the rename: %s, want %s", code, props, want)
		}
	}
	var source string
	if err := pool.QueryRow(ctx, `SELECT source_property FROM model.dimension_def WHERE id=$1::uuid`, groupedID).Scan(&source); err != nil || source != "factor" {
		t.Errorf("grouped dimension's source_property %q (err %v), want factor", source, err)
	}

	// Retype only, by the property's id: the name is kept.
	if _, _, err := run("update_dimension_property", map[string]any{"dimension_id": dimID, "property": segID, "data_type": "number"}); err != nil {
		t.Fatalf("retype seg: %v", err)
	}

	// A formula reads the renamed property; the old name is refused.
	mustRun("create_metric", map[string]any{"name": "revenue", "is_input": true, "format": "number"})
	if _, _, err := run("create_metric", map[string]any{"name": "scaled", "formula": "revenue * region.factor", "format": "number"}); err != nil {
		t.Errorf("a formula reading the renamed property: %v", err)
	}
	if _, _, err := run("create_metric", map[string]any{"name": "stale", "formula": "revenue * region.fact", "format": "number"}); err == nil || !strings.Contains(err.Error(), "UNKNOWN_PROPERTY") {
		t.Errorf("the old name: err %v, want UNKNOWN_PROPERTY", err)
	}

	// Renaming a property a formula reads rewrites the formula, there and
	// back; deleting it while a formula reads it is refused.
	scaledFormula := func() string {
		t.Helper()
		var f string
		if err := pool.QueryRow(ctx, `
			SELECT m.formula FROM model.metric_def m
			JOIN model.dimension_def d ON d.model_id = m.model_id AND d.revision_id IS NOT DISTINCT FROM m.revision_id
			WHERE d.id=$1::uuid AND m.name='scaled'`, dimID).Scan(&f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	for _, step := range []struct{ from, to, want string }{
		{"factor", "multiplier", "revenue * region.multiplier"},
		{"multiplier", "factor", "revenue * region.factor"},
	} {
		res, _, err := run("update_dimension_property", map[string]any{"dimension_id": dimID, "property": step.from, "name": step.to})
		if err != nil {
			t.Fatalf("rename %s -> %s while read: %v", step.from, step.to, err)
		}
		if !strings.Contains(res, "1 formula(s) rewritten") {
			t.Errorf("rename %s -> %s result %q, want one formula rewritten", step.from, step.to, res)
		}
		if got := scaledFormula(); got != step.want {
			t.Errorf("after renaming %s -> %s the formula is %q, want %q", step.from, step.to, got, step.want)
		}
	}
	if _, _, err := run("delete_dimension_property", map[string]any{"dimension_id": dimID, "property": "factor"}); err == nil ||
		!strings.Contains(err.Error(), metricformula.CodePropertyInUse) || !strings.Contains(err.Error(), "scaled") {
		t.Errorf("deleting a property a formula reads: err %v, want %s naming scaled", err, metricformula.CodePropertyInUse)
	}

	// Delete: the declaration goes, and a formula naming it is refused.
	if _, _, err := run("delete_dimension_property", map[string]any{"dimension_id": dimID, "property": "tier"}); err != nil {
		t.Fatalf("delete tier: %v", err)
	}
	if _, _, err := run("delete_dimension_property", map[string]any{"dimension_id": dimID, "property": "tier"}); err == nil {
		t.Error("deleting an already deleted property must be refused")
	}
	if _, _, err := run("create_metric", map[string]any{"name": "tiered", "formula": "revenue * region.tier", "format": "number"}); err == nil || !strings.Contains(err.Error(), "UNKNOWN_PROPERTY") {
		t.Errorf("a deleted property: err %v, want UNKNOWN_PROPERTY", err)
	}

	// Another dimension's property, and another model's dimension and
	// property, are out of reach — and left untouched.
	productID := mustRun("create_dimension", map[string]any{"name": "product"})
	colourID := mustRun("add_dimension_property", map[string]any{"dimension_id": productID, "name": "colour"})
	otherModel := seedModel(t, pool)
	otherRev := seedRevision(t, pool, otherModel, "Other")
	otherExec := aiassistant.NewWriteExecutor(pool, otherModel, otherRev)
	_, otherDim, err := otherExec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "elsewhere"}))
	if err != nil {
		t.Fatal(err)
	}
	_, otherProp, err := otherExec.Execute(ctx, "add_dimension_property", mustJSON(t, map[string]any{"dimension_id": otherDim, "name": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		what, dim, prop, want string
	}{
		{"a property of another dimension", dimID, colourID, "different dimension"},
		{"a property of another model's dimension", dimID, otherProp, "different model"},
		{"another model's dimension", otherDim, "x", "different model"},
	} {
		for _, tool := range []string{"update_dimension_property", "delete_dimension_property"} {
			if _, _, err := run(tool, map[string]any{"dimension_id": tc.dim, "property": tc.prop, "name": "renamed"}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s via %s: err %v, want %q", tc.what, tool, err, tc.want)
			}
		}
	}

	var declared string
	if err := pool.QueryRow(ctx, `
		SELECT string_agg(d.name || '.' || p.name || ':' || p.data_type, ',' ORDER BY d.name, p.name)
		FROM model.dimension_property p JOIN model.dimension_def d ON d.id = p.dimension_id
		WHERE d.model_id IN ($1::uuid, $2::uuid)`, modelID, otherModel).Scan(&declared); err != nil {
		t.Fatal(err)
	}
	if declared != "elsewhere.x:text,product.colour:text,region.factor:number,region.seg:number" {
		t.Errorf("declarations %q", declared)
	}
}

// TestCrossRevisionPropertyIDIsNotMisdirected pins the resolution of a
// property id from another revision. The read tools list the ACTIVE
// revision's ids before a draft exists, so a proposal's steps naturally
// carry them; there is no lineage column, so the draft counterpart is found
// by name. A rename earlier in the proposal, followed by a new declaration
// under the old name, must not re-point the id at the new declaration
// (found live: the delete removed the newly declared property and left the
// renamed one).
func TestCrossRevisionPropertyIDIsNotMisdirected(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revA := seedRevision(t, pool, modelID, "Rev A")
	ctx := context.Background()
	execA := aiassistant.NewWriteExecutor(pool, modelID, revA)
	dimA, err := func() (string, error) {
		_, id, err := execA.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
			"name":    "region",
			"members": []map[string]any{{"code": "EMEA", "label": "EMEA", "properties": map[string]string{"fact": "2"}}},
		}))
		return id, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	_, propA, err := execA.Execute(ctx, "add_dimension_property", mustJSON(t, map[string]any{
		"dimension_id": dimA, "name": "fact", "data_type": "number",
	}))
	if err != nil {
		t.Fatal(err)
	}
	draft := func(name string) string {
		t.Helper()
		_, id, err := execA.Execute(ctx, "create_revision", mustJSON(t, map[string]any{"name": name, "source_revision_id": revA}))
		if err != nil {
			t.Fatalf("create_revision: %v", err)
		}
		return id
	}
	declared := func(rev string) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `
			SELECT coalesce(string_agg(p.name || ':' || p.data_type, ',' ORDER BY p.name), '')
			FROM model.dimension_property p JOIN model.dimension_def d ON d.id = p.dimension_id
			WHERE d.revision_id=$1::uuid AND d.name='region'`, rev).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	step := func(ex *aiassistant.WriteExecutor, tool string, params map[string]any) (string, error) {
		t.Helper()
		res, _, err := ex.Execute(ctx, tool, mustJSON(t, params))
		return res, err
	}

	// The live scenario, in one proposal: rename P, declare a new property
	// under P's old name, delete P. P keeps meaning the renamed property.
	revB := draft("Draft B")
	exB := aiassistant.NewWriteExecutor(pool, modelID, revB)
	res, err := step(exB, "update_dimension_property", map[string]any{"dimension_id": dimA, "property": propA, "name": "x"})
	if err != nil {
		t.Fatalf("rename by active id: %v", err)
	}
	if !strings.Contains(res, "matched to this revision's property") {
		t.Errorf("rename by another revision's id should say it was matched: %s", res)
	}
	if _, err := step(exB, "add_dimension_property", map[string]any{"dimension_id": dimA, "name": "fact", "data_type": "text"}); err != nil {
		t.Fatal(err)
	}
	res, err = step(exB, "delete_dimension_property", map[string]any{"dimension_id": dimA, "property": propA})
	if err != nil {
		t.Fatalf("delete by active id: %v", err)
	}
	if !strings.Contains(res, "Property 'x' deleted") {
		t.Errorf("delete by the active id must reach the renamed property x: %s", res)
	}
	if got := declared(revB); got != "fact:text" {
		t.Errorf("draft B declares %q, want only the new fact:text", got)
	}
	if got := declared(revA); got != "fact:number" {
		t.Errorf("the active revision changed: %q", got)
	}

	// A later proposal (a new executor) on the same draft: the draft's
	// "fact" was declared after the copy, so the active id is refused
	// rather than matched to it.
	exB2 := aiassistant.NewWriteExecutor(pool, modelID, revB)
	for _, tc := range []struct {
		tool   string
		params map[string]any
	}{
		{"delete_dimension_property", map[string]any{"dimension_id": dimA, "property": propA}},
		{"update_dimension_property", map[string]any{"dimension_id": dimA, "property": propA, "data_type": "date"}},
	} {
		if _, err := step(exB2, tc.tool, tc.params); err == nil || !strings.Contains(err.Error(), "current name") {
			t.Errorf("%s by the active id after a re-declaration: err = %v, want a refusal asking for the current name", tc.tool, err)
		}
	}
	if got := declared(revB); got != "fact:text" {
		t.Errorf("a refused step changed draft B: %q", got)
	}

	// A rename by NAME before the id is first used: the draft's names no
	// longer match the active revision's, so the id is refused.
	revC := draft("Draft C")
	exC := aiassistant.NewWriteExecutor(pool, modelID, revC)
	if _, err := step(exC, "update_dimension_property", map[string]any{"dimension_id": dimA, "property": "fact", "name": "y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := step(exC, "add_dimension_property", map[string]any{"dimension_id": dimA, "name": "fact", "data_type": "text"}); err != nil {
		t.Fatal(err)
	}
	if _, err := step(exC, "delete_dimension_property", map[string]any{"dimension_id": dimA, "property": propA}); err == nil || !strings.Contains(err.Error(), "current name") {
		t.Errorf("delete by the active id after a rename by name: err = %v, want a refusal", err)
	}
	if got := declared(revC); got != "fact:text,y:number" {
		t.Errorf("draft C declares %q, want fact:text,y:number", got)
	}

	// An untouched draft still takes the active id, matched by name.
	revD := draft("Draft D")
	exD := aiassistant.NewWriteExecutor(pool, modelID, revD)
	if _, err := step(exD, "update_dimension_property", map[string]any{"dimension_id": dimA, "property": propA, "data_type": "text"}); err != nil {
		t.Fatalf("retype by the active id on an untouched draft: %v", err)
	}
	if got := declared(revD); got != "fact:text" {
		t.Errorf("draft D declares %q, want fact:text", got)
	}
}
