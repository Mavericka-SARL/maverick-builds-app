package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/reconcile"
)

// reconcileFixture is a small plan — item (A, B) × scen (BASE, HIGH; never
// added up) with price per item and scenario, qty per item, revenue = price
// × qty — built through the developer API, and a helper running the
// revision's reconciliation to its report.
type reconcileFixture struct {
	*roundTripFixture
	call    func(method, path string, body any) (int, string)
	item    string
	scen    string
	metrics map[string]string
}

func newReconcileFixture(t *testing.T) *reconcileFixture {
	t.Helper()
	f := &reconcileFixture{roundTripFixture: setupRoundTripFixture(t), metrics: map[string]string{}}
	dev := "rollup-test-approver"
	rev := f.workingRevID
	f.call = func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
	}
	must := func(method, path string, body any) string {
		t.Helper()
		status, raw := f.call(method, path, body)
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		return out.ID
	}
	f.item = must("POST", "/api/developer/dimensions", map[string]any{"name": "item", "revision_id": rev, "dimension_type": "standard"})
	for _, c := range []string{"A", "B"} {
		must("POST", "/api/developer/dimensions/"+f.item+"/members", map[string]any{"code": c, "label": "Item " + c})
	}
	f.scen = must("POST", "/api/developer/dimensions", map[string]any{"name": "scen", "revision_id": rev, "dimension_type": "standard", "agg_rule": "first"})
	for _, c := range []string{"BASE", "HIGH"} {
		must("POST", "/api/developer/dimensions/"+f.scen+"/members", map[string]any{"code": c, "label": c})
	}
	grid := must("POST", "/api/developer/grids", map[string]any{"name": "Plan", "revision_id": rev})
	for _, d := range []string{f.item, f.scen} {
		must("POST", "/api/developer/grids/"+grid+"/dimensions/"+d, nil)
	}
	for _, m := range []struct{ name, formula string }{{"price", ""}, {"qty", ""}, {"revenue", "price * qty"}} {
		body := map[string]any{"name": m.name, "is_input": m.formula == "", "revision_id": rev}
		if m.formula != "" {
			body["formula"] = m.formula
		}
		f.metrics[m.name] = must("POST", "/api/developer/metrics", body)
		must("POST", "/api/developer/grids/"+grid+"/metrics/"+f.metrics[m.name], nil)
	}
	write := func(metric string, codes map[string]string, v float64) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": f.metrics[metric], "dim_codes": codes, "value": v})
		if status, raw := f.call("POST", "/api/cells", json.RawMessage(body)); status != http.StatusOK {
			t.Fatalf("write %s: %d %s", metric, status, raw)
		}
	}
	for _, w := range []struct {
		item, scen string
		price      float64
	}{{"A", "BASE", 10}, {"B", "BASE", 20}, {"A", "HIGH", 15}, {"B", "HIGH", 20}} {
		write("price", map[string]string{f.item: w.item, f.scen: w.scen}, w.price)
	}
	for _, s := range []string{"BASE", "HIGH"} {
		write("qty", map[string]string{f.item: "A", f.scen: s}, 2)
		write("qty", map[string]string{f.item: "B", f.scen: s}, 3)
	}
	ctx := context.Background()
	for i := 0; i < 200 && calculation.RecalcPending(ctx, f.pool, rev); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	return f
}

// run saves the workbook, map and cases, runs the reconciliation and waits
// for its report.
func (f *reconcileFixture) run(t *testing.T, workbook []byte, m reconcile.Map, cases []reconcile.Case) *reconcile.Saved {
	t.Helper()
	rev := f.workingRevID
	if status, raw := f.call("PUT", "/api/developer/reconciliation/workbook", map[string]any{"revision_id": rev, "file_name": "plan.xlsx",
		"data_base64": base64.StdEncoding.EncodeToString(workbook)}); status != http.StatusOK || !strings.Contains(raw, `"Plan"`) {
		t.Fatalf("workbook: %d %s", status, raw)
	}
	if status, raw := f.call("PUT", "/api/developer/reconciliation/map", map[string]any{"revision_id": rev, "map": m, "cases": cases}); status != http.StatusOK {
		t.Fatalf("map: %d %s", status, raw)
	}
	if status, raw := f.call("POST", "/api/developer/reconciliation/run", map[string]any{"revision_id": rev}); status != http.StatusAccepted {
		t.Fatalf("run: %d %s", status, raw)
	}
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		status, raw := f.call("GET", "/api/developer/reconciliation?revision_id="+rev, nil)
		if status != http.StatusOK {
			t.Fatalf("get: %d %s", status, raw)
		}
		var out struct {
			Reconciliation reconcile.Saved `json:"reconciliation"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		if out.Reconciliation.Status != "running" {
			if out.Reconciliation.Status != "done" || out.Reconciliation.Report == nil {
				t.Fatalf("the run ended %s: %s", out.Reconciliation.Status, out.Reconciliation.Error)
			}
			return &out.Reconciliation
		}
	}
	t.Fatal("the run did not finish")
	return nil
}

// planMap: the Plan sheet's rows 2–3 — item in column A, price B, qty C,
// revenue D — in the case's scenario.
func planMap() reconcile.Map {
	return reconcile.Map{Rules: []reconcile.Rule{{Name: "Plan", Sheet: "Plan", Rows: &reconcile.Span{From: 2, To: 3},
		MetricsByColumn: map[string]string{"B": "price", "C": "qty", "D": "revenue"},
		Members:         map[string]reconcile.Binding{"item": {Column: "A"}, "scen": {Case: true}}}}}
}

func planWorkbook(t *testing.T, rows [][]any, formulas bool) []byte {
	t.Helper()
	x := excelize.NewFile()
	if err := x.SetSheetName("Sheet1", "Plan"); err != nil {
		t.Fatal(err)
	}
	for i, h := range []string{"Item", "Price", "Qty", "Revenue"} {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = x.SetCellValue("Plan", cell, h)
	}
	for r, row := range rows {
		for c, v := range row {
			cell, _ := excelize.CoordinatesToCellName(c+1, r+2)
			if s, ok := v.(string); ok && strings.HasPrefix(s, "=") && formulas {
				_ = x.SetCellFormula("Plan", cell, s[1:])
				continue
			}
			_ = x.SetCellValue("Plan", cell, v)
		}
	}
	var buf bytes.Buffer
	if err := x.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestReconcileSavedValues: the values a workbook was saved with, compared
// cell by cell in its case's scenario — a difference with the cell, the
// members, the workbook's and the model's value; a member the map cannot
// find is a mapping problem, not a difference; the CSV lists the
// differences; a copy of the revision and a model export keep the
// workbook, map and cases (2026-11-21).
func TestReconcileSavedValues(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	// Revenue of B is 61 in the workbook, 60 in the model.
	wb := planWorkbook(t, [][]any{{"A", 10, 2, 20}, {"B", 20, 3, 61}}, false)
	saved := f.run(t, wb, planMap(), []reconcile.Case{{Name: "Base", Members: map[string]string{"scen": "BASE"}}})
	rep := saved.Report
	if rep.Compared != 6 || rep.Matched != 5 || rep.DifferencesTotal != 1 {
		t.Fatalf("report: %d / %d, %d differences: %+v", rep.Matched, rep.Compared, rep.DifferencesTotal, rep)
	}
	d := rep.Differences[0]
	if d.Cell != "D3" || d.Metric != "revenue" || d.Expected != "61" || d.Actual != "60" || d.Members["item"] != "B" || d.Members["scen"] != "BASE" {
		t.Errorf("difference: %+v", d)
	}
	status, raw := f.call("GET", "/api/developer/reconciliation/differences.csv?revision_id="+f.workingRevID, nil)
	if status != http.StatusOK || !strings.Contains(raw, "Base,Plan,D3,revenue,item=B; scen=BASE,61,60") {
		t.Errorf("csv: %d %s", status, raw)
	}

	// The proposal finds the table: the header names the metrics, column A
	// the items.
	status, raw = f.call("POST", "/api/developer/reconciliation/propose", map[string]any{"revision_id": f.workingRevID})
	if status != http.StatusOK || !strings.Contains(raw, `"B":"price"`) || !strings.Contains(raw, `"item":{"column":"A"}`) || !strings.Contains(raw, "scen is not bound") {
		t.Errorf("proposal: %d %s", status, raw)
	}

	// A label names the member as its code does; an unknown one is a
	// mapping problem.
	wb = planWorkbook(t, [][]any{{"Item A", 10, 2, 20}, {"C", 20, 3, 60}}, false)
	rep = f.run(t, wb, planMap(), []reconcile.Case{{Name: "Base", Members: map[string]string{"scen": "BASE"}}}).Report
	if rep.Compared != 3 || rep.Matched != 3 || len(rep.MappingProblems) != 1 || !strings.Contains(rep.MappingProblems[0], `A3 "C" names no member of item`) {
		t.Errorf("labels and a mapping problem: %+v", rep)
	}

	// A map that does not hold together is refused at save.
	bad := planMap()
	bad.Rules[0].MetricsByRow = []string{"price"}
	if status, raw := f.call("PUT", "/api/developer/reconciliation/map", map[string]any{"revision_id": f.workingRevID, "map": bad}); status != http.StatusBadRequest {
		t.Errorf("an inconsistent map must be refused: %d %s", status, raw)
	}

	// A copy of the revision and an export keep it.
	if status, raw := f.call("POST", "/api/developer/revisions", map[string]any{"name": "Copy", "source_revision_id": f.workingRevID}); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("copy: %d %s", status, raw)
	}
	var copied int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM model.reconciliation r JOIN model.revision v ON v.id = r.revision_id
		WHERE v.name = 'Copy' AND r.workbook IS NOT NULL AND jsonb_array_length(r.map->'rules') = 1`).Scan(&copied)
	if copied != 1 {
		t.Errorf("a revision copy must keep the reconciliation")
	}
	pkg, err := modeltransfer.CollectExport(ctx, f.pool, f.modelID, f.workingRevID, "Working")
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Reconciliation == nil || pkg.Reconciliation.WorkbookBase64 == "" || !strings.Contains(string(pkg.Reconciliation.Map), `"Plan"`) {
		t.Errorf("an export must carry the reconciliation: %+v", pkg.Reconciliation)
	}
}

// TestReconcileRecalculatedCase: a case that changes the workbook's inputs
// is recalculated by LibreOffice before it is compared (skipped where
// LibreOffice is not installed).
func TestReconcileRecalculatedCase(t *testing.T) {
	soffice, err := exec.LookPath("soffice")
	if err != nil {
		t.Skip("LibreOffice (soffice) is not installed")
	}
	f := newReconcileFixture(t)
	testSoffice = soffice
	t.Cleanup(func() { testSoffice = "" })
	// Formulas, calculated once by LibreOffice for their saved values.
	raw := planWorkbook(t, [][]any{{"A", 10, 2, "=B2*C2"}, {"B", 20, 3, "=B3*C3"}}, true)
	cases := []reconcile.Case{
		{Name: "Base", Members: map[string]string{"scen": "BASE"}},
		{Name: "High", Members: map[string]string{"scen": "HIGH"}, Edits: []reconcile.Edit{{Cell: "Plan!B2", Value: 15}}},
		{Name: "Nowhere", Members: map[string]string{"scen": "HIGH"}, Edits: []reconcile.Edit{{Range: "Plan!B2:B3", CopyFrom: "Plan!C2:C3"}}},
	}
	saved := f.run(t, raw, planMap(), cases[:2])
	rep := saved.Report
	// Base: the saved values hold no results yet (excelize writes none):
	// its revenue reads blank; High is recalculated: 15 × 2 = 30, 20 × 3.
	if len(rep.Cases) != 2 || rep.Cases[1].Status != "compared" {
		t.Fatalf("cases: %+v", rep.Cases)
	}
	if rep.Cases[1].Compared != 6 || rep.Cases[1].Matched != 6 {
		t.Errorf("the recalculated case: %d / %d %+v", rep.Cases[1].Matched, rep.Cases[1].Compared, rep.Differences)
	}
	// Copying prices from the quantities: 2 × 2 = 4 ≠ 30 — the edits are
	// what LibreOffice calculates.
	rep = f.run(t, raw, planMap(), cases[2:]).Report
	if rep.Cases[0].Matched == rep.Cases[0].Compared {
		t.Errorf("a copied range must change the calculation: %+v", rep.Cases[0])
	}
}
