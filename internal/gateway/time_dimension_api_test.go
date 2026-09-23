package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// Time dimensions through the developer API (spec §12, "API and schema
// tests"): the type is explicit and immutable, periods are validated as a
// set and ordered by the server, a dimension merely called "month" never
// acts as time, grids refuse two time dimensions, publication refuses a
// time-series metric off its axis, and the grid payload carries the new
// fields.
func TestTimeDimensionAPI(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"

	post := func(path string, body any) (int, map[string]any) {
		status, raw := doAs(t, f, "POST", path, dev, f.appID, body)
		var out map[string]any
		_ = json.Unmarshal([]byte(raw), &out)
		if out == nil {
			out = map[string]any{"_raw": raw}
		}
		return status, out
	}

	// Omitted dimension_type defaults to standard for API compatibility.
	status, out := post("/api/developer/dimensions", map[string]any{"name": "month", "revision_id": f.workingRevID})
	if status != http.StatusOK {
		t.Fatalf("create standard 'month': %d %v", status, out)
	}
	plainMonthID := out["id"].(string)

	// A time dimension needs granularity AND fiscal month.
	if status, out = post("/api/developer/dimensions", map[string]any{"name": "period", "revision_id": f.workingRevID, "dimension_type": "time"}); status != http.StatusBadRequest {
		t.Errorf("time without granularity must be 400: %d %v", status, out)
	}
	if status, out = post("/api/developer/dimensions", map[string]any{"name": "period", "revision_id": f.workingRevID, "dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 13}); status != http.StatusBadRequest {
		t.Errorf("fiscal month 13 must be 400: %d %v", status, out)
	}
	if status, out = post("/api/developer/dimensions", map[string]any{"name": "period", "revision_id": f.workingRevID, "dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1, "parent_dimension_id": f.deptsDimID}); status != http.StatusBadRequest {
		t.Errorf("time with a parent dimension must be 400: %d %v", status, out)
	}
	// An arbitrarily named time dimension works.
	status, out = post("/api/developer/dimensions", map[string]any{"name": "fiscal_period", "revision_id": f.workingRevID, "dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 4})
	if status != http.StatusOK {
		t.Fatalf("create time dimension: %d %v", status, out)
	}
	timeID := out["id"].(string)

	// Immutable after creation, whichever path tries.
	if _, err := f.pool.Exec(ctx, `UPDATE model.dimension_def SET dimension_type='standard' WHERE id=$1::uuid`, timeID); err == nil {
		t.Error("dimension_type must be immutable (trigger)")
	}
	if _, err := f.pool.Exec(ctx, `UPDATE model.dimension_def SET time_granularity='day' WHERE id=$1::uuid`, timeID); err == nil {
		t.Error("time_granularity must be immutable (trigger)")
	}
	if status, _ := doAs(t, f, "PATCH", "/api/developer/dimensions/"+timeID, dev, f.appID, map[string]any{"name": "fiscal_period", "parent_dimension_id": f.deptsDimID}); status != http.StatusBadRequest {
		t.Errorf("giving a time dimension a parent must be 400: %d", status)
	}

	// Members: a dated member is a leaf period, an undated one an aggregate
	// period; leaves are validated as a set.
	members := "/api/developer/dimensions/" + timeID + "/members"
	if status, out = post(members, map[string]any{"code": "FY26", "label": "Fiscal 2026"}); status != http.StatusOK {
		t.Fatalf("undated member is an aggregate period and must be accepted: %d %v", status, out)
	}
	fyID := out["id"].(string)
	if status, out = post(members, map[string]any{"code": "half", "label": "x", "period_start": "2026-05-01"}); status != http.StatusBadRequest {
		t.Errorf("one date without the other must be 400: %d %v", status, out)
	}
	if status, out = post(members, map[string]any{"code": "bad", "label": "x", "period_start": "2026-05-10", "period_end": "2026-06-09"}); status != http.StatusBadRequest {
		t.Errorf("mid-month period must be 400: %d %v", status, out)
	}
	if status, out = post(members, map[string]any{"code": "2026-06", "label": "Jun", "period_start": "2026-06-01", "period_end": "2026-06-30", "parent_member_id": fyID}); status != http.StatusOK {
		t.Fatalf("add Jun under FY26: %d %v", status, out)
	}
	junID := out["id"].(string)
	if status, out = post(members, map[string]any{"code": "2026-04", "label": "Apr", "period_start": "2026-04-01", "period_end": "2026-04-30"}); status != http.StatusBadRequest {
		t.Errorf("Apr with May missing is a gap and must be 400: %d %v", status, out)
	}
	if status, out = post(members, map[string]any{"code": "2026-05", "label": "May", "period_start": "2026-05-01", "period_end": "2026-05-31"}); status != http.StatusOK {
		t.Fatalf("add May: %d %v", status, out)
	}
	if status, out = post(members, map[string]any{"code": "dup", "label": "dup", "period_start": "2026-05-01", "period_end": "2026-05-31"}); status != http.StatusBadRequest {
		t.Errorf("duplicate period must be 400: %d %v", status, out)
	}
	if status, out = post(members, map[string]any{"code": "2026-05b", "label": "x", "period_start": "2026-05-15", "period_end": "2026-06-14"}); status != http.StatusBadRequest {
		t.Errorf("overlapping period must be 400: %d %v", status, out)
	}
	if status, out = post(members, map[string]any{"code": "child", "label": "x", "period_start": "2026-07-01", "period_end": "2026-07-31", "parent_member_id": junID}); status != http.StatusBadRequest {
		t.Errorf("a leaf under a dated (leaf) period must be 400: %d %v", status, out)
	}
	if status, _ := doAs(t, f, "PATCH", members+"/"+fyID, dev, f.appID, map[string]any{"code": "FY26", "label": "Fiscal 2026", "period_start": "2026-01-01", "period_end": "2026-12-31"}); status != http.StatusBadRequest {
		t.Errorf("giving an aggregate with children its own dates must be 400: %d", status)
	}
	// Bulk generator: fills Jul..Sep through the same validation, skipping existing codes.
	if status, out = post(members+"/generate", map[string]any{"start": "2026-07-01", "end": "2026-09-30"}); status != http.StatusOK || out["created"].(float64) != 3 {
		t.Fatalf("generate: %d %v", status, out)
	}
	if status, out = post(members+"/generate", map[string]any{"start": "2026-07-15", "end": "2026-09-30"}); status != http.StatusBadRequest {
		t.Errorf("generator off a month boundary must be 400: %d %v", status, out)
	}
	// Standard dimensions must not carry dates.
	if status, out = post("/api/developer/dimensions/"+plainMonthID+"/members", map[string]any{"code": "2026-01", "label": "Jan", "period_start": "2026-01-01", "period_end": "2026-01-31"}); status != http.StatusBadRequest {
		t.Errorf("dates on a standard member must be 400: %d %v", status, out)
	}
	if status, out = post("/api/developer/dimensions/"+plainMonthID+"/members", map[string]any{"code": "2026-01", "label": "Jan"}); status != http.StatusOK {
		t.Fatalf("add standard member: %d %v", status, out)
	}

	// The developer listing returns the fields and chronological order.
	status, raw := doAs(t, f, "GET", "/api/developer/dimensions?revision_id="+f.workingRevID, dev, f.appID, nil)
	if status != http.StatusOK {
		t.Fatalf("list dimensions: %d %s", status, raw)
	}
	var dims []devDimension
	if err := json.Unmarshal([]byte(raw), &dims); err != nil {
		t.Fatal(err)
	}
	var timeDim *devDimension
	for i := range dims {
		if dims[i].ID == timeID {
			timeDim = &dims[i]
		}
		if dims[i].ID == plainMonthID && dims[i].DimensionType != "standard" {
			t.Errorf("'month' must be standard, got %q", dims[i].DimensionType)
		}
	}
	if timeDim == nil || timeDim.DimensionType != "time" || timeDim.TimeGranularity == nil || *timeDim.TimeGranularity != "month" || timeDim.FiscalYearStart == nil || *timeDim.FiscalYearStart != 4 {
		t.Fatalf("time dimension fields: %+v", timeDim)
	}
	var codes []string
	for _, m := range timeDim.Members {
		if m.Code == "FY26" {
			if m.TimeIndex != nil || m.PeriodStart != nil {
				t.Errorf("aggregate period must carry no ordinal or dates: %+v", m)
			}
			continue
		}
		if m.TimeIndex == nil || *m.TimeIndex != len(codes) || m.PeriodStart == nil {
			t.Errorf("member %s: time_index/period missing or out of order: %+v", m.Code, m)
		}
		codes = append(codes, m.Code)
	}
	if strings.Join(codes, ",") != "2026-05,2026-06,2026-07,2026-08,2026-09" {
		t.Errorf("leaves must be chronological: %v", codes)
	}

	// Deleting a period closes the ordinal gap.
	if status, _ := doAs(t, f, "DELETE", members+"/"+junID, dev, f.appID, nil); status != http.StatusOK {
		t.Fatalf("delete Jun: %d", status)
	}
	var idx []int
	rows, _ := f.pool.Query(ctx, `SELECT time_index FROM model.dimension_member WHERE dimension_id=$1::uuid AND time_index IS NOT NULL ORDER BY time_index`, timeID)
	for rows.Next() {
		var i int
		_ = rows.Scan(&i)
		idx = append(idx, i)
	}
	rows.Close()
	if fmt.Sprint(idx) != "[0 1 2 3]" {
		t.Errorf("time_index must stay dense after a delete: %v", idx)
	}

	// Grid configuration: two time dimensions on one grid are refused.
	status, out = post("/api/developer/dimensions", map[string]any{"name": "week", "revision_id": f.workingRevID, "dimension_type": "time", "time_granularity": "week", "fiscal_year_start_month": 1})
	if status != http.StatusOK {
		t.Fatalf("create second time dim: %d %v", status, out)
	}
	weekID := out["id"].(string)
	var gridID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Time grid', $2::uuid) RETURNING id::text`, f.modelID, f.workingRevID).Scan(&gridID); err != nil {
		t.Fatal(err)
	}
	if status, _ := doAs(t, f, "POST", "/api/developer/grids/"+gridID+"/dimensions/"+timeID, dev, f.appID, nil); status != http.StatusOK {
		t.Fatalf("attach time dim: %d", status)
	}
	if status, raw := doAs(t, f, "POST", "/api/developer/grids/"+gridID+"/dimensions/"+weekID, dev, f.appID, nil); status != http.StatusBadRequest || !strings.Contains(raw, "MULTIPLE_TIME_DIMENSIONS") {
		t.Errorf("second time dim on a grid must be 400 MULTIPLE_TIME_DIMENSIONS: %d %s", status, raw)
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.grid_dimension WHERE grid_id=$1::uuid`, gridID).Scan(&n)
	if n != 1 {
		t.Errorf("rejected attachment must roll back, got %d grid dims", n)
	}

	// A time-series formula saves (syntax/reference analysis) with its
	// dependency window recorded; dynamic offsets are refused.
	status, out = post("/api/developer/metrics", map[string]any{"name": "sales", "is_input": true, "revision_id": f.workingRevID})
	if status != http.StatusOK {
		t.Fatalf("create sales: %d %v", status, out)
	}
	salesID := out["id"].(string)
	if status, raw := doAs(t, f, "POST", "/api/developer/metrics", dev, f.appID, map[string]any{"name": "bad", "is_input": false, "formula": "LAG(sales, sales, 0)", "revision_id": f.workingRevID}); status != http.StatusBadRequest || !strings.Contains(raw, "DYNAMIC_TIME_OFFSET_UNSUPPORTED") {
		t.Errorf("dynamic offset must be 400 DYNAMIC_TIME_OFFSET_UNSUPPORTED: %d %s", status, raw)
	}
	if status, raw := doAs(t, f, "POST", "/api/developer/metrics", dev, f.appID, map[string]any{"name": "bad", "is_input": false, "formula": "TIMESUM(sales)", "revision_id": f.workingRevID}); status != http.StatusBadRequest {
		t.Errorf("later-parity function must stay unknown: %d %s", status, raw)
	}
	status, out = post("/api/developer/metrics", map[string]any{"name": "mov3", "is_input": false, "formula": "MOVINGSUM(sales, -2, 0, AVERAGE)", "revision_id": f.workingRevID, "time_summary": "last"})
	if status != http.StatusOK {
		t.Fatalf("create mov3: %d %v", status, out)
	}
	movID := out["id"].(string)
	var minOff, maxOff int
	var ts string
	if err := f.pool.QueryRow(ctx, `SELECT d.min_time_offset, d.max_time_offset, m.time_summary FROM model.calc_dependency d JOIN model.metric_def m ON m.id=d.metric_id WHERE d.metric_id=$1::uuid`, movID).Scan(&minOff, &maxOff, &ts); err != nil {
		t.Fatalf("dependency row: %v", err)
	}
	if minOff != -2 || maxOff != 0 || ts != "last" {
		t.Errorf("dependency window / summary: [%d,%d] %s, want [-2,0] last", minOff, maxOff, ts)
	}
	if status, raw := doAs(t, f, "POST", "/api/developer/metrics", dev, f.appID, map[string]any{"name": "bad", "is_input": true, "revision_id": f.workingRevID, "time_summary": "median"}); status != http.StatusBadRequest {
		t.Errorf("unknown time_summary must be 400: %d %s", status, raw)
	}

	// Publication: the time-series metric is not on a time-dimensioned
	// grid yet → TIME_DIMENSION_REQUIRED. Once it (and its input) are, the
	// revision activates.
	if status, raw := doAs(t, f, "PUT", "/api/developer/revisions/"+f.workingRevID+"/activate", dev, f.appID, nil); status != http.StatusBadRequest || !strings.Contains(raw, "TIME_DIMENSION_REQUIRED") {
		t.Errorf("activate with a stranded time-series metric must be 400 TIME_DIMENSION_REQUIRED: %d %s", status, raw)
	}
	// Putting it on the STANDARD "month" grid does not help: that dimension is not time.
	var plainGridID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Plain grid', $2::uuid) RETURNING id::text`, f.modelID, f.workingRevID).Scan(&plainGridID); err != nil {
		t.Fatal(err)
	}
	if status, _ := doAs(t, f, "POST", "/api/developer/grids/"+plainGridID+"/dimensions/"+plainMonthID, dev, f.appID, nil); status != http.StatusOK {
		t.Fatalf("attach month: %d", status)
	}
	if status, raw := doAs(t, f, "POST", "/api/developer/grids/"+plainGridID+"/metrics/"+movID, dev, f.appID, nil); status != http.StatusBadRequest || !strings.Contains(raw, "TIME_DIMENSION_REQUIRED") {
		t.Errorf("a standard dimension named month must not satisfy a time-series metric: %d %s", status, raw)
	}
	for _, mid := range []string{salesID, movID} {
		if status, raw := doAs(t, f, "POST", "/api/developer/grids/"+gridID+"/metrics/"+mid, dev, f.appID, nil); status != http.StatusOK {
			t.Fatalf("attach metric to time grid: %d %s", status, raw)
		}
	}
	if status, raw := doAs(t, f, "PUT", "/api/developer/revisions/"+f.workingRevID+"/activate", dev, f.appID, nil); status != http.StatusOK {
		t.Errorf("activate with a well-placed time-series metric: %d %s", status, raw)
	}

	// The grid payload carries the time fields, members in chronological order.
	status, raw = doAs(t, f, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, gridID), dev, f.appID, nil)
	if status != http.StatusOK {
		t.Fatalf("grid: %d %s", status, raw)
	}
	var grid struct {
		Dimensions []gridDimension `json:"dimensions"`
		Metrics    []metricRow     `json:"metrics"`
	}
	if err := json.Unmarshal([]byte(raw), &grid); err != nil {
		t.Fatal(err)
	}
	if len(grid.Dimensions) != 1 || grid.Dimensions[0].DimensionType != "time" || grid.Dimensions[0].TimeGranularity == nil {
		t.Fatalf("grid dimension time fields: %+v", grid.Dimensions)
	}
	if got := grid.Dimensions[0].Members; len(got) != 5 || got[0].Code != "2026-05" || got[0].TimeIndex == nil || *got[3].TimeIndex != 3 || got[4].Code != "FY26" || got[4].ParentCode != "" {
		t.Errorf("grid members: leaves chronological with time_index, then the aggregate: %+v", got)
	}
	for _, m := range grid.Metrics {
		if m.ID == movID && m.TimeSummary != "last" {
			t.Errorf("grid metric time_summary: %q", m.TimeSummary)
		}
	}

	// A time-member edit recalculates every metric on the axis (spec §8.3):
	// with sales only in September, adding October gives mov3 a new leaf
	// there — the trailing three-period average (0 + 30 + 0) / 3.
	if status, raw := doAs(t, f, "POST", "/api/cells", dev, f.appID, map[string]any{
		"model_id": f.modelID, "metric_id": salesID, "revision_id": f.workingRevID,
		"dim_codes": map[string]string{timeID: "2026-09"}, "value": 30}); status != http.StatusOK {
		t.Fatalf("write sales: %d %s", status, raw)
	}
	awaitRow := func(metricID, code string, want float64) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var v float64
			err := f.pool.QueryRow(ctx, `SELECT value FROM runtime.calc_result WHERE metric_id=$1::uuid AND dim_members=$2::jsonb ORDER BY calc_at DESC LIMIT 1`,
				metricID, fmt.Sprintf(`{"%s":"%s"}`, timeID, code)).Scan(&v)
			if err == nil && math.Abs(v-want) < 1e-6 {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Errorf("%s @ %s never reached %v", metricID, code, want)
	}
	awaitRow(movID, "2026-09", 10) // (Jul 0 + Aug 0 + Sep 30) / 3
	// June was deleted above, so the calendar has a gap: the next insert
	// reports it rather than silently extending a broken axis ...
	if status, out = post(members, map[string]any{"code": "2026-10", "label": "Oct", "period_start": "2026-10-01", "period_end": "2026-10-31"}); status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out), "gap") {
		t.Fatalf("insert with an existing gap must be 400 naming the gap: %d %v", status, out)
	}
	// ... and healing the gap makes the insert legal.
	if status, out = post(members, map[string]any{"code": "2026-06", "label": "Jun", "period_start": "2026-06-01", "period_end": "2026-06-30"}); status != http.StatusOK {
		t.Fatalf("re-add Jun: %d %v", status, out)
	}
	if status, out = post(members, map[string]any{"code": "2026-10", "label": "Oct", "period_start": "2026-10-01", "period_end": "2026-10-31"}); status != http.StatusOK {
		t.Fatalf("add Oct: %d %v", status, out)
	}
	awaitRow(movID, "2026-10", 10) // (Aug 0 + Sep 30 + Oct 0) / 3 — a row that exists only because the insert recalculated

	// CSV import of a time dimension: whole file validated together.
	if status, raw := doAs(t, f, "POST", "/api/import/dimension-members", dev, f.appID, map[string]any{
		"dimension_id": weekID, "csv": "code,label,period_start,period_end\nW1,Week 1,2026-01-05,2026-01-11\nW2,Week 2,2026-01-12,2026-01-18\n"}); status != http.StatusOK {
		t.Fatalf("csv import weeks: %d %s", status, raw)
	}
	if status, raw := doAs(t, f, "POST", "/api/import/dimension-members", dev, f.appID, map[string]any{
		"dimension_id": weekID, "csv": "code,label,period_start,period_end\nW3,Week 3,2026-01-19,2026-01-24\n"}); status != http.StatusBadRequest {
		t.Errorf("csv with a 6-day week must be 400 and roll back: %d %s", status, raw)
	}
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid`, weekID).Scan(&n)
	if n != 2 {
		t.Errorf("rejected csv must leave the dimension untouched, got %d members", n)
	}
}

// TestTimeDimensionPackageRoundTrip: a model export → import into another
// model keeps the time marker, every period's exact dates and order, the
// metric's time summary and the dependency offsets — so a time-series model
// transferred between tenants calculates identically.
func TestTimeDimensionPackageRoundTrip(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"

	status, out := doAs(t, f.rollupFixture, "POST", "/api/developer/dimensions", dev, f.appID, map[string]any{
		"name": "month", "revision_id": f.workingRevID, "dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 7})
	if status != http.StatusOK {
		t.Fatalf("create time dim: %d %s", status, out)
	}
	var created map[string]any
	_ = json.Unmarshal([]byte(out), &created)
	monthID := created["id"].(string)
	if status, out := doAs(t, f.rollupFixture, "POST", "/api/developer/dimensions/"+monthID+"/members/generate", dev, f.appID,
		map[string]any{"start": "2026-01-01", "end": "2026-03-31"}); status != http.StatusOK {
		t.Fatalf("generate: %d %s", status, out)
	}
	status, out = doAs(t, f.rollupFixture, "POST", "/api/developer/metrics", dev, f.appID, map[string]any{
		"name": "sales", "is_input": true, "revision_id": f.workingRevID, "time_summary": "last"})
	if status != http.StatusOK {
		t.Fatalf("create sales: %d %s", status, out)
	}
	status, out = doAs(t, f.rollupFixture, "POST", "/api/developer/metrics", dev, f.appID, map[string]any{
		"name": "trailing", "is_input": false, "formula": "MOVINGSUM(sales, -1, 0) + LAG(sales, 2, 0)", "revision_id": f.workingRevID, "time_summary": "average"})
	if status != http.StatusOK {
		t.Fatalf("create trailing: %d %s", status, out)
	}

	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("export status = %d, body = %v", status, pkg)
	}
	status, res := f.do(t, "POST", "/api/admin/models/import", f.taPersona,
		map[string]any{"application_id": f.appID, "model_name": "Imported time", "package": pkg})
	if status != http.StatusOK {
		t.Fatalf("import status = %d, body = %v", status, res)
	}
	newModel, _ := res["model_id"].(string)
	newRev, _ := res["revision_id"].(string)

	var dimType, gran string
	var fiscal int
	if err := f.pool.QueryRow(ctx, `SELECT dimension_type, time_granularity, fiscal_year_start_month FROM model.dimension_def
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='month'`, newModel, newRev).Scan(&dimType, &gran, &fiscal); err != nil {
		t.Fatalf("imported dimension: %v", err)
	}
	if dimType != "time" || gran != "month" || fiscal != 7 {
		t.Errorf("imported time marker = %s/%s/%d, want time/month/7", dimType, gran, fiscal)
	}
	rows, err := f.pool.Query(ctx, `SELECT m.code, m.period_start::text, m.period_end::text, m.time_index
		FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id
		WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name='month' ORDER BY m.time_index`, newModel, newRev)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var code, ps, pe string
		var idx int
		_ = rows.Scan(&code, &ps, &pe, &idx)
		got = append(got, fmt.Sprintf("%d:%s:%s..%s", idx, code, ps, pe))
	}
	rows.Close()
	want := "0:2026-01:2026-01-01..2026-01-31,1:2026-02:2026-02-01..2026-02-28,2:2026-03:2026-03-01..2026-03-31"
	if strings.Join(got, ",") != want {
		t.Errorf("imported periods:\n got %v\nwant %s", got, want)
	}
	var salesSummary, trailingSummary string
	var minOff, maxOff int
	if err := f.pool.QueryRow(ctx, `SELECT s.time_summary, tr.time_summary, d.min_time_offset, d.max_time_offset
		FROM model.metric_def tr
		JOIN model.calc_dependency d ON d.metric_id = tr.id
		JOIN model.metric_def s ON s.id = d.depends_on_metric_id
		WHERE tr.model_id=$1::uuid AND tr.revision_id=$2::uuid AND tr.name='trailing'`, newModel, newRev).Scan(&salesSummary, &trailingSummary, &minOff, &maxOff); err != nil {
		t.Fatalf("imported dependency: %v", err)
	}
	if salesSummary != "last" || trailingSummary != "average" || minOff != -2 || maxOff != 0 {
		t.Errorf("imported summaries/offsets = %s/%s [%d,%d], want last/average [-2,0]", salesSummary, trailingSummary, minOff, maxOff)
	}
}

// TestScopedSeriesSuppressesHiddenWindow (spec §10): on a scoped read a
// time-series metric is served from persisted rows, and any cell whose
// source window reaches a period hidden from the caller is withheld — the
// hidden value is neither shown nor "omitted from the arithmetic".
func TestScopedSeriesSuppressesHiddenWindow(t *testing.T) {
	timeID := "t"
	periods := []string{"2026-01", "2026-02", "2026-03", "2026-04"}
	// The caller's lattice already lacks the hidden February.
	dims := map[string]*rollup.Dimension{timeID: {ID: timeID, IsTime: true, Members: []rollup.Member{
		{Code: "2026-01", TimeIndex: 0}, {Code: "2026-03", TimeIndex: 2}, {Code: "2026-04", TimeIndex: 3},
	}}}
	rows := map[string]float64{}
	for i, code := range periods {
		b, _ := json.Marshal(map[string]string{timeID: code})
		rows[string(b)] = float64(10 * (i + 1))
	}
	// PREVIOUS(x): window [-1, -1].
	prev := &scopedSeries{TimeDimID: timeID, TimeSummary: "sum", Periods: periods, Hidden: map[string]bool{"2026-02": true},
		Rows: rows, MinOffset: -1, MaxOffset: -1}
	universe := []metricRow{{ID: "m", Name: "prev", Formula: strPtr("PREVIOUS(x)"), AggRule: "sum"}}
	cells, totals := scopeCalcCells(context.Background(), dims, map[string][]string{"m": {timeID}}, map[string]string{timeID: "t"},
		universe, map[string]float64{}, map[string]*scopedSeries{"m": prev})
	if _, ok := cells["m:2026-03"]; ok {
		t.Error("March reads the hidden February through PREVIOUS and must be suppressed")
	}
	if v, ok := cells["m:2026-01"]; !ok || v != 10 {
		t.Errorf("January's window (December) touches nothing hidden: got %v ok=%v", v, ok)
	}
	if v, ok := cells["m:2026-04"]; !ok || v != 40 {
		t.Errorf("April reads March, visible: got %v ok=%v", v, ok)
	}
	if totals["m"] != 50 {
		t.Errorf("total over the served cells (Jan + Apr) = %v, want 50", totals["m"])
	}
	// CUMULATE(x): unbounded past — everything after the hidden period goes.
	cum := &scopedSeries{TimeDimID: timeID, TimeSummary: "last", Periods: periods, Hidden: map[string]bool{"2026-02": true}, Rows: rows, UnbPast: true}
	cells, totals = scopeCalcCells(context.Background(), dims, map[string][]string{"m": {timeID}}, map[string]string{timeID: "t"},
		universe, map[string]float64{}, map[string]*scopedSeries{"m": cum})
	if len(cells) != 1 || cells["m:2026-01"] != 10 || totals["m"] != 10 {
		t.Errorf("unbounded-past series after a hidden period must be withheld: cells=%v totals=%v", cells, totals)
	}
	// No hidden period: everything is served and the time summary applies.
	open := &scopedSeries{TimeDimID: timeID, TimeSummary: "last", Periods: periods, Rows: rows, MinOffset: -1, MaxOffset: -1}
	full := map[string]*rollup.Dimension{timeID: {ID: timeID, IsTime: true, Members: []rollup.Member{
		{Code: "2026-01", TimeIndex: 0}, {Code: "2026-02", TimeIndex: 1}, {Code: "2026-03", TimeIndex: 2}, {Code: "2026-04", TimeIndex: 3},
	}}}
	cells, totals = scopeCalcCells(context.Background(), full, map[string][]string{"m": {timeID}}, map[string]string{timeID: "t"},
		universe, map[string]float64{}, map[string]*scopedSeries{"m": open})
	if len(cells) != 4 || totals["m"] != 40 {
		t.Errorf("unhidden series: cells=%v totals=%v (want 4 cells, last=40)", cells, totals)
	}
}

func strPtr(s string) *string { return &s }
