package gateway

// The cell write refuses coordinates no reader reads — a parent member
// (stored values there were ignored by the grid but counted by a pinned
// KPI), a calculated member, a dimension of another revision — through the
// resolver it now shares with the gRPC write and form postings. A member
// another dimension points at (a department its staff belong to) is not a
// parent: the grid lets a planner type there, and the file import now
// accepts the same value.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestCellWriteCoordinates(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f, method, path, dev, f.appID, body)
	}
	write := func(metricID string, dims map[string]string) (int, string) {
		t.Helper()
		return call("POST", "/api/cells", map[string]any{
			"model_id": f.modelID, "revision_id": f.workingRevID, "metric_id": metricID,
			"dim_codes": dims, "value": 7,
		})
	}
	refused := func(what string, status int, raw, code string) {
		t.Helper()
		var body struct{ Code string }
		_ = json.Unmarshal([]byte(raw), &body)
		if status != http.StatusBadRequest || body.Code != code {
			t.Errorf("%s: %d %s, want 400 %s", what, status, raw, code)
		}
	}

	status, raw := call("POST", "/api/developer/dimensions/"+f.deptsDimID+"/members",
		map[string]any{"code": "DEPT_GAP", "label": "Gap", "formula": "{DEPT_A} - {DEPT_B}"})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("create calculated member: %d %s", status, raw)
	}
	var otherDim string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, revision_id, name)
		VALUES ($1::uuid, $2::uuid, 'regions') RETURNING id::text`, f.modelID, f.annualRevID).Scan(&otherDim); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'WEST', 'West')`, otherDim); err != nil {
		t.Fatal(err)
	}

	before := factCount(t, f)
	status, raw = write(f.amountMetricID, map[string]string{f.staffDimID: "STAFF_A1", f.regionsDimID: "REGION_GROUP"})
	refused("parent member", status, raw, "NOT_LEAF")
	status, raw = write(f.amountMetricID, map[string]string{f.staffDimID: "STAFF_A1", f.regionsDimID: "NORTH"})
	refused("unknown code", status, raw, "UNKNOWN_MEMBER")
	status, raw = write(f.quotaMetricID, map[string]string{f.deptsDimID: "DEPT_GAP"})
	refused("calculated member", status, raw, "CALCULATED_MEMBER")
	status, raw = write(f.amountMetricID, map[string]string{otherDim: "WEST"})
	refused("another revision's dimension", status, raw, "UNKNOWN_DIMENSION")
	status, raw = write(f.amountMetricID, map[string]string{"regions": "WEST"})
	refused("a name for a dimension id", status, raw, "UNKNOWN_DIMENSION")
	if n := factCount(t, f); n != before {
		t.Errorf("refused writes stored %d facts", n-before)
	}

	// DEPT_A has staff pointing at it, but no member of departments under it.
	if status, raw := write(f.quotaMetricID, map[string]string{f.deptsDimID: "DEPT_A"}); status != http.StatusOK {
		t.Errorf("write at a department with staff: %d %s, want 200", status, raw)
	}
	if status, raw := write(f.amountMetricID, map[string]string{f.staffDimID: "STAFF_A1", f.regionsDimID: "WEST"}); status != http.StatusOK {
		t.Errorf("write at a leaf: %d %s, want 200", status, raw)
	}

	// The import takes the same value at the same department.
	xlsx := workbookBase64(t, [][]string{{"departments", "quota"}, {"DEPT_A", "9"}})
	status, body := f.do(t, "POST", "/api/import/upload", dev, map[string]any{"xlsx_base64": xlsx, "revision_id": f.workingRevID})
	if status != http.StatusOK {
		b, _ := json.Marshal(body)
		if !strings.Contains(string(b), "NOT_LEAF") {
			t.Fatalf("import at a department with staff: %d %s", status, b)
		}
		t.Errorf("import at a department with staff: %d %s, want 200", status, b)
	}
}

func factCount(t *testing.T, f *rollupFixture) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, f.modelID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func workbookBase64(t *testing.T, rows [][]string) string {
	t.Helper()
	wb := excelize.NewFile()
	defer wb.Close() //nolint:errcheck
	sheet := wb.GetSheetName(0)
	for i, row := range rows {
		for j, cell := range row {
			ref, _ := excelize.CoordinatesToCellName(j+1, i+1)
			_ = wb.SetCellValue(sheet, ref, cell)
		}
	}
	buf, err := wb.WriteToBuffer()
	if err != nil {
		t.Fatalf("write workbook: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
