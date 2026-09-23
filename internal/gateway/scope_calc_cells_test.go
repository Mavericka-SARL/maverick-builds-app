// Pure, DB-free unit tests for toRollupDims and scopeCalcCells — both
// operate entirely on already-loaded, in-memory data (no SQL of their own),
// so unlike generic_rollup_workflow_test.go's fixture these don't need
// testcontainers at all. See scopeCalcCells's own doc comment in handler.go
// for why it exists (a real access-control leak in the naive "filter
// calc_result rows by the caller's hidden dims" approach, plus a numeric
// divergence for non-linear formulas the old scopeCalcTotals had).
package gateway

import (
	"context"
	"encoding/json"
	"testing"
)

func TestToRollupDims(t *testing.T) {
	parentID := "dim-region"
	sourceID := "dim-staff"
	sourceProp := "region"
	dims := []gridDimension{
		{ID: "dim-dept", Name: "department", Members: []gridDimMember{
			{ID: "m-a", Code: "A", ParentCode: ""},
			{ID: "m-b", Code: "B", ParentCode: "A"},
		}},
		{ID: parentID, Name: "region", SourceDimensionID: &sourceID, SourceProperty: &sourceProp, Members: []gridDimMember{
			{ID: "m-west", Code: "WEST", Properties: json.RawMessage(`{"tier":"1"}`)},
		}},
		{ID: "dim-child", Name: "child", ParentDimensionID: &parentID},
	}

	out := toRollupDims(dims)

	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	dept := out["dim-dept"]
	if dept == nil {
		t.Fatal("dim-dept missing")
	}
	if dept.ParentDimensionID != "" || dept.SourceDimensionID != "" || dept.SourceProperty != "" {
		t.Errorf("dept should have no relation fields set, got %+v", dept)
	}
	if len(dept.Members) != 2 || dept.Members[1].ParentCode != "A" {
		t.Errorf("dept.Members = %+v, want B with ParentCode=A", dept.Members)
	}

	region := out[parentID]
	if region.SourceDimensionID != sourceID || region.SourceProperty != sourceProp {
		t.Errorf("region SourceDimensionID/SourceProperty = %q/%q, want %q/%q", region.SourceDimensionID, region.SourceProperty, sourceID, sourceProp)
	}
	if len(region.Members) != 1 || region.Members[0].Properties["tier"] != "1" {
		t.Errorf("region.Members[0].Properties = %+v, want tier=1", region.Members[0].Properties)
	}

	child := out["dim-child"]
	if child.ParentDimensionID != parentID {
		t.Errorf("child.ParentDimensionID = %q, want %q", child.ParentDimensionID, parentID)
	}
}

// TestScopeCalcCells exercises the full recomputation chain without any
// database: a same-grain calc dependency (tax, on revenue), a calc-on-calc
// dependency (capped, on tax), and a non-linear formula (capped's IF) all in
// one pass, over a hand-built two-department universe.
//
//	revenue: A=1000, B=2000
//	tax = revenue*0.1:        A=100,  B=200
//	capped = IF(tax>150,150,tax): A=100 (not capped), B=150 (capped)
//	capped's CombineAgg(sum) total = 250 — proving the total comes from
//	summing per-combo results, not from evaluating the formula once against
//	a scalar tax total (100+200=300 -> IF(300>150,150,300)=150, which would
//	give a wrong total of 150 instead of 250).
func TestScopeCalcCells(t *testing.T) {
	deptID := "dim-dept"
	dims := toRollupDims([]gridDimension{
		{ID: deptID, Name: "department", Members: []gridDimMember{
			{ID: "m-a", Code: "A"},
			{ID: "m-b", Code: "B"},
		}},
	})
	dimIDToName := map[string]string{deptID: "department"}

	revenueID, taxID, cappedID := "m-revenue", "m-tax", "m-capped"
	taxFormula := "=revenue*0.1"
	cappedFormula := "=IF(tax>150,150,tax)"
	universe := []metricRow{
		{ID: revenueID, Name: "revenue", IsInput: true, AggRule: "sum"},
		{ID: taxID, Name: "tax", IsInput: false, Formula: &taxFormula, AggRule: "sum"},
		{ID: cappedID, Name: "capped", IsInput: false, Formula: &cappedFormula, AggRule: "sum"},
	}
	metricDimIDs := map[string][]string{
		revenueID: {deptID},
		taxID:     {deptID},
		cappedID:  {deptID},
	}
	scopedInputCells := map[string]float64{
		revenueID + ":A": 1000,
		revenueID + ":B": 2000,
	}

	cells, totals := scopeCalcCells(context.Background(), dims, metricDimIDs, dimIDToName, universe, scopedInputCells, nil)

	if v := cells[taxID+":A"]; v != 100 {
		t.Errorf("tax[A] = %v, want 100", v)
	}
	if v := cells[taxID+":B"]; v != 200 {
		t.Errorf("tax[B] = %v, want 200", v)
	}
	if v := totals[taxID]; v != 300 {
		t.Errorf("tax total = %v, want 300", v)
	}
	if v := cells[cappedID+":A"]; v != 100 {
		t.Errorf("capped[A] = %v, want 100 (tax=100, not capped)", v)
	}
	if v := cells[cappedID+":B"]; v != 150 {
		t.Errorf("capped[B] = %v, want 150 (tax=200, capped)", v)
	}
	if v := totals[cappedID]; v != 250 {
		t.Errorf("capped total = %v, want 250 (100+150 per-combo, not 150 from evaluating once against the scalar tax total of 300)", v)
	}
}
