package gateway

// A metric's display label ("R&D", "EBITDA Margin %") is what a P&L line is
// called; a snake_case name cannot spell it, and the label used to be derived
// from the name only. It is set through the developer API, shown by every
// reader (developer model, /api/metrics, /api/grid, chart series), and carried
// by revision duplication and model export/import. Clearing it falls back to
// the derived label.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestMetricDisplayLabel(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"

	call := func(method, path string, body any) string {
		t.Helper()
		status, raw := doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
		if status != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		return raw
	}
	type labelled struct {
		ID       string `json:"id"`
		MetricID string `json:"metric_id"`
		Name     string `json:"name"`
		Label    string `json:"label"`
		LabelSet bool   `json:"label_set"`
	}
	find := func(where string, list []labelled, id string) labelled {
		t.Helper()
		for _, m := range list {
			if m.ID == id || m.MetricID == id {
				return m
			}
		}
		t.Fatalf("%s: metric %s not listed", where, id)
		return labelled{}
	}
	devModel := func(id string) labelled {
		t.Helper()
		var out struct {
			Metrics []labelled `json:"metrics"`
		}
		_ = json.Unmarshal([]byte(call("GET", "/api/developer/model?revision_id="+f.workingRevID, nil)), &out)
		return find("developer model", out.Metrics, id)
	}

	// ── Created with a label ──
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(call("POST", "/api/developer/metrics", map[string]any{
		"name": "r_and_d", "label": "  R&D ", "is_input": true, "revision_id": f.workingRevID})), &created)
	if m := devModel(created.ID); m.Label != "R&D" || !m.LabelSet {
		t.Errorf("created: label=%q set=%v, want \"R&D\" set", m.Label, m.LabelSet)
	}

	// ── A label added later reaches every reader ──
	call("PATCH", "/api/developer/metrics/"+f.amountMetricID, map[string]any{"label": "Spend (k$)"})
	if m := devModel(f.amountMetricID); m.Label != "Spend (k$)" || !m.LabelSet || m.Name != "amount" {
		t.Errorf("patched: %+v, want label \"Spend (k$)\" set and the name kept", m)
	}
	var listed []labelled
	_ = json.Unmarshal([]byte(call("GET", "/api/metrics?revision_id="+f.workingRevID, nil)), &listed)
	if m := find("/api/metrics", listed, f.amountMetricID); m.Label != "Spend (k$)" {
		t.Errorf("/api/metrics label = %q", m.Label)
	}
	var grid struct {
		Metrics []labelled `json:"metrics"`
	}
	_ = json.Unmarshal([]byte(call("GET", "/api/grid?grid_id="+f.gridStaffID, nil)), &grid)
	if m := find("/api/grid", grid.Metrics, f.amountMetricID); m.Label != "Spend (k$)" {
		t.Errorf("/api/grid label = %q", m.Label)
	}
	_, chartWidgetID := seedChartDashboard(t, f.rollupFixture, "Label dash")
	var chart struct {
		Series []labelled `json:"series"`
	}
	_ = json.Unmarshal([]byte(call("POST", "/api/dashboard-widgets/"+chartWidgetID+"/chart-data",
		map[string]any{"context": map[string]string{}})), &chart)
	if len(chart.Series) == 0 || chart.Series[0].Label != "Spend (k$)" {
		t.Errorf("chart series = %+v, want the stored label", chart.Series)
	}

	// ── A PATCH without "label" keeps it ──
	call("PATCH", "/api/developer/metrics/"+f.amountMetricID, map[string]any{"format": "currency"})
	if m := devModel(f.amountMetricID); m.Label != "Spend (k$)" {
		t.Errorf("label lost by a PATCH that did not carry it: %q", m.Label)
	}

	// ── Revision duplication and export/import carry it ──
	assertCopied := func(where, modelID, revID string) {
		t.Helper()
		var label *string
		if err := f.pool.QueryRow(ctx, `SELECT label FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='amount'`, modelID, revID).Scan(&label); err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		if label == nil || *label != "Spend (k$)" {
			t.Errorf("%s: label = %v", where, label)
		}
	}
	var rev struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(call("POST", "/api/developer/revisions", map[string]any{"name": "Labelled copy", "source_revision_id": f.workingRevID})), &rev)
	assertCopied("duplicated revision", f.modelID, rev.ID)

	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("export: %d %v", status, pkg)
	}
	status, res := f.do(t, "POST", "/api/admin/models/import", f.taPersona,
		map[string]any{"application_id": f.appID, "model_name": "Imported labels", "package": pkg})
	if status != http.StatusOK {
		t.Fatalf("import: %d %v", status, res)
	}
	assertCopied("imported model", res["model_id"].(string), res["revision_id"].(string))

	// ── "" clears it back to the label derived from the name ──
	call("PATCH", "/api/developer/metrics/"+f.amountMetricID, map[string]any{"label": ""})
	if m := devModel(f.amountMetricID); m.LabelSet || m.Label != toLabel("amount") {
		t.Errorf("cleared: %+v, want the derived label %q", m, toLabel("amount"))
	}
}
