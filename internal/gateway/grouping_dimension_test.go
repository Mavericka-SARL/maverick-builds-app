// Property-grouping dimensions over the developer's HTTP API: a developer
// creates "area" grouping "employees" by their declared area property,
// derives its members from the values, reads a grid by area totals, and
// sees SUMIFS(salary, area, "North") recalculated when an employee's area
// changes — and every invalid grouping is refused with INVALID_GROUPING.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// awaitCalc polls calc_result for metric at dims (dimension id -> code).
func (f *dimFormulaFixture) awaitCalc(metricID string, dims map[string]string, want float64) {
	f.t.Helper()
	raw, _ := json.Marshal(dims)
	deadline := time.Now().Add(15 * time.Second)
	var last float64
	seen := false
	for time.Now().Before(deadline) {
		var v float64
		if err := f.pool.QueryRow(context.Background(), `
			SELECT value FROM runtime.calc_result
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND dim_members=$4::jsonb
		`, f.modelID, f.revID, metricID, string(raw)).Scan(&v); err == nil {
			last, seen = v, true
			if nearly(v, want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !seen {
		f.t.Errorf("metric %s at %s: no calc_result row (want %v)", metricID, raw, want)
		return
	}
	f.t.Errorf("metric %s at %s = %v, want %v (waited 15s)", metricID, raw, last, want)
}

// gridCells reads a grid as the developer: "metricID:code" -> value.
func (f *dimFormulaFixture) gridCells(gridID string) map[string]float64 {
	f.t.Helper()
	q := url.Values{}
	q.Set("grid_def_id", gridID)
	q.Set("revision_id", f.revID)
	status, raw := f.req("GET", "/api/grid?"+q.Encode(), nil)
	if status != http.StatusOK {
		f.t.Fatalf("GET /api/grid: %d %s", status, raw)
	}
	var resp struct {
		Cells map[string]float64 `json:"cells"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		f.t.Fatalf("decode grid: %v", err)
	}
	return resp.Cells
}

func TestPropertyGroupingDimensionOverHTTP(t *testing.T) {
	f := setupDimFormulaFixture(t)
	dims := "/api/developer/dimensions"

	// employees with a declared text property area.
	emp := f.call("POST", dims, map[string]any{"name": "employees", "revision_id": f.revID})
	f.call("POST", dims+"/"+emp+"/properties", map[string]any{"name": "area", "data_type": "text"})
	f.call("POST", dims+"/"+emp+"/properties", map[string]any{"name": "grade", "data_type": "number"})
	empMember := map[string]string{}
	area := map[string]string{"E1": "North", "E2": "South", "E3": "North"}
	for _, code := range []string{"E1", "E2", "E3"} {
		empMember[code] = f.call("POST", dims+"/"+emp+"/members", map[string]any{"code": code, "label": code})
		f.call("PATCH", dims+"/"+emp+"/members/"+empMember[code], map[string]any{
			"code": code, "label": code, "properties": map[string]string{"area": area[code]}})
	}
	month := f.call("POST", dims, map[string]any{"name": "month", "revision_id": f.revID,
		"dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1})

	// Refusals, before the grouping exists.
	code := metricformula.CodeInvalidGrouping
	for _, tc := range []struct {
		what string
		body map[string]any
	}{
		{"an undeclared property", map[string]any{"source_dimension_id": emp, "source_property": "zone"}},
		{"a property without a source", map[string]any{"source_property": "area"}},
		{"a source without a property", map[string]any{"source_dimension_id": emp}},
		{"a time source", map[string]any{"source_dimension_id": month, "source_property": "area"}},
		{"a time grouping", map[string]any{"source_dimension_id": emp, "source_property": "area",
			"dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1}},
		{"a parent dimension and a grouping", map[string]any{"source_dimension_id": emp, "source_property": "area",
			"parent_dimension_id": f.region}},
		{"a source that does not exist", map[string]any{"source_dimension_id": "00000000-0000-0000-0000-000000000000", "source_property": "area"}},
		{"derive_members without a grouping", map[string]any{"derive_members": true}},
	} {
		body := map[string]any{"name": "bad", "revision_id": f.revID}
		for k, v := range tc.body {
			body[k] = v
		}
		f.refused(tc.what, "POST", dims, body, code)
	}

	// The grouping: the property named in another case is stored as
	// declared, and derive_members adds North and South.
	status, raw := f.req("POST", dims, map[string]any{"name": "area", "revision_id": f.revID,
		"source_dimension_id": emp, "source_property": "AREA", "derive_members": true})
	if status != http.StatusOK {
		t.Fatalf("create area: %d %s", status, raw)
	}
	var created struct {
		ID      string   `json:"id"`
		Derived []string `json:"derived_members"`
	}
	_ = json.Unmarshal(raw, &created)
	areaDim := created.ID
	if strings.Join(created.Derived, ",") != "North,South" {
		t.Errorf("derived_members = %v, want [North South]", created.Derived)
	}
	status, raw = f.req("GET", dims+"?revision_id="+f.revID, nil)
	var listed []struct {
		ID                string  `json:"id"`
		SourceDimensionID *string `json:"source_dimension_id"`
		SourceProperty    *string `json:"source_property"`
		Members           []struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		} `json:"members"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &listed) != nil {
		t.Fatalf("list dimensions: %d %s", status, raw)
	}
	areaMember := map[string]string{}
	for _, d := range listed {
		if d.ID != areaDim {
			continue
		}
		if deref(d.SourceDimensionID) != emp || deref(d.SourceProperty) != "area" {
			t.Errorf("area lists source %q property %q, want %s / area", deref(d.SourceDimensionID), deref(d.SourceProperty), emp)
		}
		for _, m := range d.Members {
			areaMember[m.Code] = m.ID
		}
	}
	if len(areaMember) != 2 {
		t.Errorf("area members %v, want North and South", areaMember)
	}

	// PATCH refusals: self, cycle, a parent on a grouping, another revision.
	f.refused("grouping a dimension by itself", "PATCH", dims+"/"+areaDim,
		map[string]any{"source_dimension_id": areaDim, "source_property": "area"}, code)
	f.call("POST", dims+"/"+areaDim+"/properties", map[string]any{"name": "lead", "data_type": "text"})
	f.refused("a grouping cycle", "PATCH", dims+"/"+emp,
		map[string]any{"source_dimension_id": areaDim, "source_property": "lead"}, code)
	f.refused("a parent dimension on a grouping", "PATCH", dims+"/"+areaDim,
		map[string]any{"parent_dimension_id": f.region}, code)
	f.refused("an undeclared property on PATCH", "PATCH", dims+"/"+areaDim,
		map[string]any{"source_property": "nope"}, code)
	otherRev := f.call("POST", "/api/developer/revisions", map[string]any{"name": "Other"})
	var otherEmp string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='employees'`, otherRev).Scan(&otherEmp); err != nil {
		t.Fatalf("the copied revision's employees: %v", err)
	}
	f.refused("a source in another revision", "POST", dims, map[string]any{"name": "area2", "revision_id": f.revID,
		"source_dimension_id": otherEmp, "source_property": "area"}, code)

	// The copied revision's area groups the COPY's employees.
	var copiedSource string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(source_dimension_id::text,'') FROM model.dimension_def WHERE revision_id=$1::uuid AND name='area'`, otherRev).Scan(&copiedSource); err != nil {
		t.Fatal(err)
	}
	if copiedSource != otherEmp {
		t.Errorf("copied area's source %s, want the copy's employees %s", copiedSource, otherEmp)
	}

	// salary by employee; north_sal = SUMIFS over the grouping; a grid by
	// area whose area_sal reads salary through it.
	salary := f.call("POST", "/api/developer/metrics", f.metricBody("salary", ""))
	staff := f.call("POST", "/api/developer/grids", map[string]any{"name": "Staff", "revision_id": f.revID})
	f.call("POST", "/api/developer/grids/"+staff+"/dimensions/"+emp, nil)
	f.call("POST", "/api/developer/grids/"+staff+"/metrics/"+salary, nil)
	northSal := f.call("POST", "/api/developer/metrics", f.metricBody("north_sal", `SUMIFS(salary, area, "North")`))
	f.call("POST", "/api/developer/grids/"+staff+"/metrics/"+northSal, nil)
	byArea := f.call("POST", "/api/developer/grids", map[string]any{"name": "By area", "revision_id": f.revID})
	f.call("POST", "/api/developer/grids/"+byArea+"/dimensions/"+areaDim, nil)
	areaSal := f.call("POST", "/api/developer/metrics", f.metricBody("area_sal", "salary"))
	f.call("POST", "/api/developer/grids/"+byArea+"/metrics/"+areaSal, nil)
	for code, v := range map[string]float64{"E1": 100, "E2": 200, "E3": 50} {
		f.call("POST", "/api/cells", map[string]any{"model_id": f.modelID, "metric_id": salary, "revision_id": f.revID,
			"dim_codes": map[string]string{emp: code}, "value": v})
	}
	f.awaitCalc(northSal, map[string]string{emp: "E1"}, 150)
	f.awaitCalc(areaSal, map[string]string{areaDim: "North"}, 150)
	f.awaitCalc(areaSal, map[string]string{areaDim: "South"}, 200)
	cells := f.gridCells(byArea)
	if !nearly(cells[areaSal+":North"], 150) || !nearly(cells[areaSal+":South"], 200) {
		t.Errorf("grid by area: North %v South %v, want 150 and 200 (cells %v)", cells[areaSal+":North"], cells[areaSal+":South"], cells)
	}

	// E2 moves to North: the member edit recalculates through the grouping.
	f.call("PATCH", dims+"/"+emp+"/members/"+empMember["E2"], map[string]any{
		"code": "E2", "label": "E2", "properties": map[string]string{"area": "North"}})
	f.awaitCalc(northSal, map[string]string{emp: "E1"}, 350)
	f.awaitCalc(areaSal, map[string]string{areaDim: "North"}, 350)

	// A new value gets its member on a re-derive (PATCH derive_members).
	f.call("PATCH", dims+"/"+emp+"/members/"+empMember["E3"], map[string]any{
		"code": "E3", "label": "E3", "properties": map[string]string{"area": "West"}})
	status, raw = f.req("PATCH", dims+"/"+areaDim, map[string]any{"derive_members": true})
	if status != http.StatusOK || !strings.Contains(string(raw), `"West"`) {
		t.Errorf("re-derive: %d %s, want West derived", status, raw)
	}
	f.awaitCalc(areaSal, map[string]string{areaDim: "West"}, 50)
	f.awaitCalc(northSal, map[string]string{emp: "E1"}, 300)

	// The property a grouping uses cannot be deleted, and clearing the
	// source while formulas read through it is refused.
	var areaPropID string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id::text FROM model.dimension_property WHERE dimension_id=$1::uuid AND name='area'`, emp).Scan(&areaPropID); err != nil {
		t.Fatal(err)
	}
	if status, raw := f.req("DELETE", dims+"/"+emp+"/properties/"+areaPropID, nil); status != http.StatusConflict ||
		!strings.Contains(string(raw), metricformula.CodePropertyInUse) {
		t.Errorf("deleting the grouping's property: %d %s, want 409 %s", status, raw, metricformula.CodePropertyInUse)
	}
	// The refusal's error string starts with its code: clients match on
	// that prefix (api/openapi.yaml, the Error schema).
	if status, raw := f.req("PATCH", dims+"/"+areaDim, map[string]any{"source_dimension_id": nil}); status != http.StatusConflict {
		t.Errorf("clearing the source while formulas read area: %d %s, want 409 %s", status, raw, metricformula.CodeDimensionInUse)
	} else {
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || !strings.HasPrefix(body.Error, metricformula.CodeDimensionInUse+": ") {
			t.Errorf("clearing the source while formulas read area: error %q, want it to start with %s:", raw, metricformula.CodeDimensionInUse)
		}
	}

	// The grouped dimension cannot be deleted while area groups it.
	if status, raw := f.req("DELETE", dims+"/"+emp, nil); status != http.StatusConflict ||
		!strings.Contains(string(raw), metricformula.CodeDimensionInUse) {
		t.Errorf("deleting the grouped dimension: %d %s, want 409 %s", status, raw, metricformula.CodeDimensionInUse)
	}

	// Switching the property (same source) regroups and recalculates:
	// grade has no values, so every group is empty.
	f.call("PATCH", dims+"/"+areaDim, map[string]any{"source_property": "grade"})
	f.awaitCalc(areaSal, map[string]string{areaDim: "North"}, 0)
	var stored []string
	rows, err := f.pool.Query(context.Background(), `SELECT source_property FROM model.dimension_def WHERE id=$1::uuid`, areaDim)
	if err == nil {
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			stored = append(stored, s)
		}
		rows.Close()
	}
	sort.Strings(stored)
	if strings.Join(stored, ",") != "grade" {
		t.Errorf("source_property after PATCH = %v, want grade", stored)
	}
}
