package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
)

// A value typed into a metric before records fed it was added to the
// records' total (a typed 100 and a posted 7 read 107) or stood alone where
// no record posted, in cells nobody may type into to clear it. The owner's
// decision of 2026-11-09: such values are set aside. The grid, charts and
// calculations read a records-fed metric's records only; nothing is
// deleted; creating a mapping says how many it sets aside; and they count
// again once no mapping feeds the metric.
func TestTypedValuesOfAPostedMetricAreSetAside(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	staffA1 := f.amountMetricID + ":STAFF_A1"
	grid := func() (cells, totals map[string]any) {
		t.Helper()
		status, body := f.do(t, "GET", fmt.Sprintf("/api/grid?grid_def_id=%s&revision_id=%s", f.gridStaffID, f.workingRevID), dev, nil)
		if status != http.StatusOK {
			t.Fatalf("grid: %d %v", status, body)
		}
		cells, _ = body["cells"].(map[string]any)
		totals, _ = body["totals"].(map[string]any)
		return cells, totals
	}
	cells, totals := grid()
	if cells[staffA1] != 7.0 || totals[f.amountMetricID] != 7.0 {
		t.Errorf("grid: STAFF_A1 = %v, total = %v; want 7 and 7, the record's posting only", cells[staffA1], totals[f.amountMetricID])
	}
	values, err := calculation.NewStore(f.pool).LoadInputValueMap(ctx, f.modelID, f.workingRevID, f.amountMetricID)
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	if len(values) != 1 || sum != 7 {
		t.Errorf("calculations read amount as %v; want the record's 7 alone", values)
	}
	typed := countRows(t, f.rollupFixture, `SELECT count(DISTINCT dim_members) FROM runtime.fact_input
		WHERE revision_id=$1::uuid AND metric_id=$2::uuid AND source_ref IS NULL`, f.workingRevID, f.amountMetricID)
	if typed == 0 {
		t.Fatal("the typed values were deleted; want them kept, set aside")
	}

	// Creating a mapping into a metric holding typed values says how many
	// it sets aside.
	status, raw := doAs(t, f.rollupFixture, "POST", "/api/developer/form-integrations?revision_id="+f.workingRevID, dev, f.appID, map[string]any{
		"form_id": f.formID, "grid_id": f.gridStaffID, "name": "again", "source_field": "amt", "target_metric_id": f.amountMetricID,
		"dimension_mappings": map[string]string{f.staffDimID: "staff"},
	})
	var created struct {
		ID            string `json:"id"`
		TypedSetAside *int   `json:"typed_set_aside"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(raw), &created) != nil || created.TypedSetAside == nil || *created.TypedSetAside != typed {
		t.Fatalf("create mapping: %d %s; want typed_set_aside = %d", status, raw, typed)
	}

	// No mapping feeds the metric any more: its typed values count again.
	for _, id := range []string{f.mappingID, created.ID} {
		if status, raw := doAs(t, f.rollupFixture, "DELETE", "/api/developer/form-integrations/"+id, dev, f.appID, nil); status != http.StatusOK {
			t.Fatalf("delete mapping: %d %s", status, raw)
		}
	}
	if cells, _ := grid(); cells[staffA1] != 100.0 {
		t.Errorf("unfed, STAFF_A1 = %v; want the typed 100 back", cells[staffA1])
	}
}
