package gateway

import (
	"context"
	"testing"
	"time"
)

// renameFormulaFixture: region EMEA (factor 2) and US (factor 3), revenue
// EMEA=100 and US=50, and metrics reading the dimension and its members on
// the grid, every one computed.
func renameFormulaFixture(t *testing.T) *dimFormulaFixture {
	f := setupDimFormulaFixture(t)
	f.call("POST", "/api/developer/dimensions/"+f.region+"/properties", map[string]any{"name": "factor", "data_type": "number"})
	f.setMember("EMEA", map[string]string{"factor": "2"})
	f.setMember("US", map[string]string{"factor": "3"})
	for name, text := range map[string]string{
		"scaled":  `revenue * region.factor`,
		"lk":      `LOOKUP(revenue, region, "EMEA")`,
		"is_emea": `IF(region = "EMEA", 1, 0)`,
		"si_emea": `SUMIFS(revenue, region, "EMEA")`,
	} {
		f.metric[name] = f.call("POST", "/api/developer/metrics", f.metricBody(name, text))
		f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric[name], nil)
	}
	f.writeCell("revenue", "US", 50)
	f.writeCell("revenue", "EMEA", 100)
	f.await("scaled", "EMEA", 200)
	f.await("lk", "US", 100)
	f.await("is_emea", "EMEA", 1)
	f.await("si_emea", "US", 100)
	return f
}

func (f *dimFormulaFixture) formulaOf(name string) string {
	f.t.Helper()
	var text string
	if err := f.pool.QueryRow(context.Background(), `SELECT formula FROM model.metric_def WHERE id=$1::uuid`, f.metric[name]).Scan(&text); err != nil {
		f.t.Fatal(err)
	}
	return text
}

// TestMemberRenameKeepsFormulasReading: a member's new code reaches the
// formulas naming it, so they keep their values. They used to keep the old
// code and go silently wrong: is_emea 0, si_emea 0 and lk with no row.
func TestMemberRenameKeepsFormulasReading(t *testing.T) {
	f := renameFormulaFixture(t)
	f.call("PATCH", "/api/developer/dimensions/"+f.region+"/members/"+f.members["EMEA"], map[string]any{"code": "EUR", "label": "Europe"})
	for name, want := range map[string]string{
		"lk":      `LOOKUP(revenue, region, "EUR")`,
		"is_emea": `IF(region = "EUR", 1, 0)`,
		"si_emea": `SUMIFS(revenue, region, "EUR")`,
	} {
		if got := f.formulaOf(name); got != want {
			t.Errorf("%s reads %q, want %q", name, got, want)
		}
	}
	// A write recalculates them: what they read now comes from the
	// formulas, not from results re-keyed with the member.
	f.writeCell("revenue", "EUR", 120)
	f.await("si_emea", "US", 120)
	f.await("lk", "US", 120)
	f.await("is_emea", "EUR", 1)
	f.await("is_emea", "US", 0)
}

// TestDimensionRenameKeepsFormulasReading: a dimension's new name reaches
// the formulas naming it, so a later write computes them as before. They
// used to keep the old name: rows went missing or read 0.
func TestDimensionRenameKeepsFormulasReading(t *testing.T) {
	f := renameFormulaFixture(t)
	f.call("PATCH", "/api/developer/dimensions/"+f.region, map[string]any{"name": "territory"})
	for name, want := range map[string]string{
		"scaled":  `revenue * territory.factor`,
		"lk":      `LOOKUP(revenue, territory, "EMEA")`,
		"is_emea": `IF(territory = "EMEA", 1, 0)`,
		"si_emea": `SUMIFS(revenue, territory, "EMEA")`,
	} {
		if got := f.formulaOf(name); got != want {
			t.Errorf("%s reads %q, want %q", name, got, want)
		}
	}
	f.writeCell("revenue", "US", 60)
	f.await("scaled", "US", 180)
	f.await("scaled", "EMEA", 200)
	f.await("si_emea", "US", 100)
	f.await("lk", "US", 100)
	f.await("is_emea", "EMEA", 1)
}

// TestMemberRenameReachesRecordsFiltersAndRuns: a member's new code reaches
// the records holding it, each posting's coordinates, the posting row
// filters naming it and the workflow runs holding it. They kept the old
// code (2026-11-09): a record reposted to a code that no longer existed, a
// row filter stopped matching, and a run's condition and lock lost it.
func TestMemberRenameReachesRecordsFiltersAndRuns(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	f.metric["spend"] = f.call("POST", "/api/developer/metrics", f.metricBody("spend", ""))
	f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["spend"], nil)
	form := q(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid, $2::uuid, 'costs', 'Costs',
		jsonb_build_array(jsonb_build_object('name','region','label','Region','type','dimension','dimension_id',$3::text),
			jsonb_build_object('name','amount','label','Amount','type','number'))) RETURNING id::text`, f.modelID, f.revID, f.region)
	mapping := q(`INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id, dimension_mappings, row_filter, posting_statuses)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'costs', 'amount', $5::uuid, jsonb_build_object($6::text, 'region'),
		        '[{"field":"region","is":["emea","US"]}]', '{draft,submitted,approved}') RETURNING id::text`, f.modelID, f.revID, form, f.gridID, f.metric["spend"], f.region)
	if status, raw := f.req("POST", "/api/forms/"+form+"/import", map[string]any{"csv": "region,amount\nEMEA,5\n"}); status != 200 {
		t.Fatalf("import: %d %s", status, raw)
	}
	record := q(`SELECT id::text FROM runtime.form_record WHERE form_id=$1::uuid`, form)
	// An import's records post after it answers.
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var at string
		if f.pool.QueryRow(ctx, `SELECT dim_members->>$2 FROM runtime.form_record_posting WHERE form_record_id=$1::uuid`, record, f.region).Scan(&at) == nil {
			if at != "EMEA" {
				t.Fatalf("the record posted at %q, want EMEA", at)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the record never posted")
		}
	}
	appID := q(`SELECT application_id::text FROM core.model WHERE id=$1::uuid`, f.modelID)
	def := q(`INSERT INTO workflow.workflow_def (application_id, revision_id, name, trigger_event, steps, status, context_schema)
		VALUES ($1::uuid, $2::uuid, 'Region sign-off', 'manual', '[]', 'published',
		        jsonb_build_array(jsonb_build_object('key','r','data_type','Dimension member','dimension_id',$3::text))) RETURNING id::text`, appID, f.revID, f.region)
	user := q(`SELECT id::text FROM identity.user LIMIT 1`)
	run := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, context, started_by) VALUES ($1::uuid, '{"r":"EMEA"}', $2::uuid) RETURNING id::text`, def, user)

	f.call("PATCH", "/api/developer/dimensions/"+f.region+"/members/"+f.members["EMEA"], map[string]any{"code": "EUR", "label": "Europe"})

	if got := q(`SELECT data->>'region' FROM runtime.form_record WHERE id=$1::uuid`, record); got != "EUR" {
		t.Errorf("the record holds %q, want EUR", got)
	}
	if got := q(`SELECT dim_members->>$2 FROM runtime.form_record_posting WHERE form_record_id=$1::uuid`, record, f.region); got != "EUR" {
		t.Errorf("the posting is at %q, want EUR", got)
	}
	if got := q(`SELECT row_filter::text FROM model.form_metric_mapping WHERE id=$1::uuid`, mapping); got != `[{"is": ["EUR", "US"], "field": "region"}]` {
		t.Errorf("the row filter is %s, want EUR and US", got)
	}
	if got := q(`SELECT context->>'r' FROM workflow.workflow_instance WHERE id=$1::uuid`, run); got != "EUR" {
		t.Errorf("the run holds %q, want EUR", got)
	}
}

// TestMemberDeleteKeepsItsChildren: deleting a member keeps its children,
// moved to the top level, with their own children under them — what the
// dialog and the manual now say (owner decision 2026-11-09). They said the
// descendants were deleted.
func TestMemberDeleteKeepsItsChildren(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()
	members := "/api/developer/dimensions/" + f.region + "/members"
	parent := f.call("POST", members, map[string]any{"code": "AMER", "label": "Americas"})
	child := f.call("POST", members, map[string]any{"code": "NA", "label": "North America", "parent_member_id": parent})
	grand := f.call("POST", members, map[string]any{"code": "NA_E", "label": "North America East", "parent_member_id": child})
	f.call("DELETE", members+"/"+parent, nil)
	parentOf := func(id string) string {
		t.Helper()
		var p string
		if err := f.pool.QueryRow(ctx, `SELECT COALESCE(parent_member_id::text, '') FROM model.dimension_member WHERE id=$1::uuid`, id).Scan(&p); err != nil {
			t.Fatalf("member %s: %v", id, err)
		}
		return p
	}
	if p := parentOf(child); p != "" {
		t.Errorf("the child's parent is %q, want none (top level)", p)
	}
	if p := parentOf(grand); p != child {
		t.Errorf("the grandchild's parent is %q, want the child", p)
	}
}
