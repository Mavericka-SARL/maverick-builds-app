package gateway

import (
	"net/http"
	"testing"
)

// A metric belongs to one revision, so editing it recalculates that revision
// and answers its value there — not one entry per revision of the model with
// data, named after the edited metric and null everywhere else
// (OBSERVATIONS "Limits met finishing the AI-built production model by hand").
func TestMetricPatchRecalculatesOnlyItsRevision(t *testing.T) {
	f := setupRoundTripFixture(t)

	status, body := f.do(t, "POST", "/api/developer/revisions", "rollup-test-approver",
		map[string]any{"name": "Copy", "source_revision_id": f.workingRevID})
	if status != http.StatusOK {
		t.Fatalf("duplicate status = %d, body = %v", status, body)
	}
	copyRev, _ := body["id"].(string)

	var workingDeptTotal, copyDeptTotal string
	f.scalar(t, &workingDeptTotal, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='dept_total'`, f.modelID, f.workingRevID)
	f.scalar(t, &copyDeptTotal, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='dept_total'`, f.modelID, copyRev)

	status, body = f.do(t, "PATCH", "/api/developer/metrics/"+workingDeptTotal, "rollup-test-approver",
		map[string]any{"format": "number"})
	if status != http.StatusOK {
		t.Fatalf("PATCH status = %d, body = %v", status, body)
	}
	recalc, _ := body["recalc"].([]any)
	if len(recalc) != 1 {
		t.Fatalf("recalc entries = %d (%v), want 1: only the metric's own revision changed", len(recalc), recalc)
	}
	entry, _ := recalc[0].(map[string]any)
	if entry["revision_id"] != f.workingRevID {
		t.Errorf("recalc revision = %v, want the metric's revision %s", entry["revision_id"], f.workingRevID)
	}
	if v, _ := entry["value"].(float64); v == 0 {
		t.Errorf("recalc value = %v, want the metric's total in its revision", entry["value"])
	}

	// Editing the copy's metric changes the copy and leaves Working alone.
	var workingBefore float64
	f.scalar(t, &workingBefore, `SELECT COALESCE(sum(value),0)::float8 FROM runtime.calc_result WHERE revision_id=$1::uuid AND metric_id=$2::uuid AND dim_members='{}'`, f.workingRevID, workingDeptTotal)
	status, body = f.do(t, "PATCH", "/api/developer/metrics/"+copyDeptTotal, "rollup-test-approver",
		map[string]any{"formula": "amount*2"})
	if status != http.StatusOK {
		t.Fatalf("PATCH copy status = %d, body = %v", status, body)
	}
	recalc, _ = body["recalc"].([]any)
	if len(recalc) != 1 {
		t.Fatalf("copy recalc entries = %d (%v), want 1", len(recalc), recalc)
	}
	entry, _ = recalc[0].(map[string]any)
	if entry["revision_id"] != copyRev {
		t.Errorf("copy recalc revision = %v, want %s", entry["revision_id"], copyRev)
	}
	if v, _ := entry["value"].(float64); v != 2*workingBefore {
		t.Errorf("copy dept_total = %v, want %v (twice Working's)", entry["value"], 2*workingBefore)
	}
	var workingAfter float64
	f.scalar(t, &workingAfter, `SELECT COALESCE(sum(value),0)::float8 FROM runtime.calc_result WHERE revision_id=$1::uuid AND metric_id=$2::uuid AND dim_members='{}'`, f.workingRevID, workingDeptTotal)
	if workingAfter != workingBefore {
		t.Errorf("Working dept_total moved from %v to %v after the copy's metric changed", workingBefore, workingAfter)
	}
}
