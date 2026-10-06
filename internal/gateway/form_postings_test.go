package gateway

// A saved csv_import integration into a form used to insert into a table no
// migration creates (every row failed, the run answered 200); it now goes
// through the form import's path, posting each record through the form's
// mappings. Deleting a mapping or a form, or retargeting a mapping, used to
// leave its posted totals in the metric, where nothing could remove them.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFormPostingsFollowRunsAndDeletes(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
	}
	// posted sums what a mapping posted into a metric, once the background
	// posting has settled on want (or the deadline passes).
	posted := func(metricID, mappingID string, want float64) float64 {
		t.Helper()
		var v float64
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if err := f.pool.QueryRow(ctx, `SELECT COALESCE(sum(value),0) FROM runtime.fact_input
				WHERE metric_id=$1::uuid AND source_ref=$2::uuid`, metricID, mappingID).Scan(&v); err != nil {
				t.Fatal(err)
			}
			if v == want || time.Now().After(deadline) {
				return v
			}
		}
	}
	if got := posted(f.amountMetricID, f.mappingID, 7); got != 7 {
		t.Fatalf("fixture posting = %v, want 7", got)
	}

	// A saved CSV run into the form creates its records and posts them.
	status, raw := call("POST", "/api/developer/integrations?revision_id="+f.workingRevID, map[string]any{
		"name": "Expenses", "type": "csv_import", "target_type": "form", "target_id": f.formID,
	})
	if status != http.StatusOK {
		t.Fatalf("create integration: %d %s", status, raw)
	}
	var intg struct{ ID string }
	_ = json.Unmarshal([]byte(raw), &intg)
	run := func(csv string) (int, string) {
		t.Helper()
		return call("POST", "/api/integrations/"+intg.ID+"/run", map[string]any{"csv": csv})
	}
	if status, raw := run("staff,amt,status\nSTAFF_A2,12,approved\nSTAFF_A1,5,approved\n"); status != http.StatusOK || !strings.Contains(raw, `"rows_imported":2`) {
		t.Fatalf("run: %d %s", status, raw)
	}
	if n := countRows(t, f.rollupFixture, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, f.formID); n != 2 {
		t.Errorf("records after the run = %d, want 2", n)
	}
	// The fixture's own posted 7 was a fact with no record behind it; the
	// re-post rebuilds the mapping's total from its records.
	if got := posted(f.amountMetricID, f.mappingID, 17); got != 17 {
		t.Errorf("posted after the run = %v, want 17 (12 + 5)", got)
	}
	// An imported submitted record fires the form's form_submit rules, as
	// one created by hand does.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO workflow.automation_rule (application_id, revision_id, name, description, trigger_type, workflow_name, enabled, workflow_def_id, source_form_id)
		VALUES ($1::uuid, $2::uuid, 'On submit', '', 'form_submit', 'Expense Approval', true, $3::uuid, $4::uuid)`,
		f.appID, f.workingRevID, f.wfID, f.formID); err != nil {
		t.Fatal(err)
	}
	instances := func() int {
		return countRows(t, f.rollupFixture, `SELECT count(*) FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`, f.wfID)
	}
	before := instances()
	if status, raw := run("staff,amt,status\nSTAFF_B1,3,submitted\n"); status != http.StatusOK {
		t.Fatalf("run of a submitted record: %d %s", status, raw)
	}
	for deadline := time.Now().Add(10 * time.Second); instances() == before && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	if n := instances(); n != before+1 {
		t.Errorf("workflow instances after importing a submitted record: %d, want %d", n, before+1)
	}

	// A row naming no member of the staff dimension is refused, and nothing
	// of that file is created.
	if status, raw := run("staff,amt,status\nSTAFF_A2,1,approved\nSTAFF_ZZ,1,approved\n"); status != http.StatusUnprocessableEntity || !strings.Contains(raw, "STAFF_ZZ") {
		t.Errorf("run naming an unknown member: %d %s, want 422 naming it", status, raw)
	}
	if n := countRows(t, f.rollupFixture, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, f.formID); n != 3 {
		t.Errorf("records after a refused run = %d, want 3", n)
	}

	// Retargeting the mapping moves its total to the new metric.
	mapping := map[string]any{
		"grid_id": f.gridStaffID, "name": "post amt", "source_field": "amt", "target_metric_id": f.budgetMetricID,
		"aggregation": "sum", "posting_statuses": []string{"approved"},
		"dimension_mappings": map[string]string{f.staffDimID: "staff"},
	}
	if status, raw := call("PATCH", "/api/developer/form-integrations/"+f.mappingID, mapping); status != http.StatusOK {
		t.Fatalf("retarget: %d %s", status, raw)
	}
	if got := posted(f.amountMetricID, f.mappingID, 0); got != 0 {
		t.Errorf("old target after the retarget = %v, want 0", got)
	}
	if got := posted(f.budgetMetricID, f.mappingID, 17); got != 17 {
		t.Errorf("new target after the retarget = %v, want 17", got)
	}

	// Deleting the mapping takes its total out.
	if status, raw := call("DELETE", "/api/developer/form-integrations/"+f.mappingID, nil); status != http.StatusOK {
		t.Fatalf("delete mapping: %d %s", status, raw)
	}
	if got := posted(f.budgetMetricID, f.mappingID, 0); got != 0 {
		t.Errorf("posted after the mapping delete = %v, want 0", got)
	}

	// Deleting the form takes its mappings' totals out too.
	var second string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id, dimension_mappings)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'post amt again', 'amt', $5::uuid, jsonb_build_object($6::text, 'staff'))
		RETURNING id::text`, f.modelID, f.workingRevID, f.formID, f.gridStaffID, f.amountMetricID, f.staffDimID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if status, raw := call("POST", "/api/developer/form-integrations/"+second+"/backfill", nil); status != http.StatusOK {
		t.Fatalf("backfill: %d %s", status, raw)
	}
	if got := posted(f.amountMetricID, second, 17); got != 17 {
		t.Fatalf("second mapping posted %v, want 17", got)
	}
	if status, raw := call("DELETE", "/api/forms/"+f.formID, nil); status != http.StatusOK {
		t.Fatalf("delete form: %d %s", status, raw)
	}
	if got := posted(f.amountMetricID, second, 0); got != 0 {
		t.Errorf("posted after the form delete = %v, want 0", got)
	}
	// The direct entry on the same cell stays.
	if n := countRows(t, f.rollupFixture, `SELECT count(*) FROM runtime.fact_input WHERE metric_id=$1::uuid AND source_ref IS NULL`, f.amountMetricID); n == 0 {
		t.Error("the direct entry went with the form")
	}
}

func countRows(t *testing.T, f *rollupFixture, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
