package rollup

import (
	"context"
	"errors"
	"testing"
)

// memFetch builds a RawValue over an in-memory table of exact combos, keyed
// by metricID + sorted "dim=code" pairs — good enough for tests since combo
// keys here always have distinct dimension IDs.
func memFetch(t *testing.T, table map[string]float64) RawValue {
	t.Helper()
	return func(_ context.Context, metricID string, combo map[string]string) (float64, bool, error) {
		key := metricID
		for _, k := range sortedKeys(combo) {
			key += "|" + k + "=" + combo[k]
		}
		v, ok := table[key]
		if !ok {
			return 0, false, nil
		}
		return v, true, nil
	}
}

func fetchKey(metricID string, combo map[string]string) string {
	key := metricID
	for _, k := range sortedKeys(combo) {
		key += "|" + k + "=" + combo[k]
	}
	return key
}

// deptDim: region-a -> {sales, ga} (departments), each a leaf.
func deptDim() *Dimension {
	return &Dimension{
		ID: "dept",
		Members: []Member{
			{ID: "region-a", Code: "region-a"},
			{ID: "ga", Code: "ga", ParentCode: "region-a"},
			{ID: "sales", Code: "sales", ParentCode: "region-a"},
		},
	}
}

func TestResolveSameDimensionSum(t *testing.T) {
	dims := map[string]*Dimension{"dept": deptDim()}
	table := map[string]float64{
		fetchKey("amount", map[string]string{"dept": "ga"}):    100,
		fetchKey("amount", map[string]string{"dept": "sales"}): 50,
	}
	fetch := memFetch(t, table)

	v, ok, err := Resolve(context.Background(), dims, "amount", []string{"dept"}, AggSum,
		map[string]string{"dept": "region-a"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true for a rollup sum")
	}
	if v != 150 {
		t.Errorf("region-a sum = %v, want 150", v)
	}
}

func TestResolveSameDimensionAverage(t *testing.T) {
	dims := map[string]*Dimension{"dept": deptDim()}
	table := map[string]float64{
		fetchKey("amount", map[string]string{"dept": "ga"}):    100,
		fetchKey("amount", map[string]string{"dept": "sales"}): 50,
	}
	fetch := memFetch(t, table)

	v, ok, err := Resolve(context.Background(), dims, "amount", []string{"dept"}, AggAverage,
		map[string]string{"dept": "region-a"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || v != 75 {
		t.Errorf("region-a average = %v, ok=%v, want 75, true", v, ok)
	}
}

func TestResolveSameDimensionCount(t *testing.T) {
	dims := map[string]*Dimension{"dept": deptDim()}
	table := map[string]float64{
		fetchKey("headcount", map[string]string{"dept": "ga"}): 3,
		// sales has no recorded headcount at all -> contributes 0, excluded from count.
	}
	fetch := memFetch(t, table)

	v, ok, err := Resolve(context.Background(), dims, "headcount", []string{"dept"}, AggCount,
		map[string]string{"dept": "region-a"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || v != 1 {
		t.Errorf("region-a count = %v, ok=%v, want 1, true", v, ok)
	}
}

func TestResolveHiddenChildExcludedFromRollup(t *testing.T) {
	// "sales" simply absent from Members, as if writeguard.ExpandHidden
	// filtered it out before the Dimension was constructed.
	dims := map[string]*Dimension{"dept": {
		ID: "dept",
		Members: []Member{
			{ID: "region-a", Code: "region-a"},
			{ID: "ga", Code: "ga", ParentCode: "region-a"},
		},
	}}
	table := map[string]float64{
		fetchKey("amount", map[string]string{"dept": "ga"}):    100,
		fetchKey("amount", map[string]string{"dept": "sales"}): 999, // must never be reachable
	}
	fetch := memFetch(t, table)

	v, ok, err := Resolve(context.Background(), dims, "amount", []string{"dept"}, AggSum,
		map[string]string{"dept": "region-a"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || v != 100 {
		t.Errorf("region-a sum with sales hidden = %v, ok=%v, want 100, true", v, ok)
	}
}

func TestResolveTrivialExactMatchPassesThroughMiss(t *testing.T) {
	dims := map[string]*Dimension{"dept": deptDim()}
	fetch := memFetch(t, map[string]float64{}) // nothing recorded anywhere

	v, ok, err := Resolve(context.Background(), dims, "amount", []string{"dept"}, AggSum,
		map[string]string{"dept": "ga"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false for a genuine miss at a leaf with no rollup involved, got value=%v", v)
	}
	if v != 0 {
		t.Errorf("expected value=0 alongside ok=false, got %v", v)
	}
}

func TestResolveTrivialExactMatchHit(t *testing.T) {
	dims := map[string]*Dimension{"dept": deptDim()}
	fetch := memFetch(t, map[string]float64{
		fetchKey("amount", map[string]string{"dept": "ga"}): 42,
	})

	v, ok, err := Resolve(context.Background(), dims, "amount", []string{"dept"}, AggSum,
		map[string]string{"dept": "ga"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || v != 42 {
		t.Errorf("got value=%v ok=%v, want 42, true", v, ok)
	}
}

// employees -> cost_centers structural chain: employees.parent_dimension_id
// == cost_centers.id (in rollup terms, the employees Dimension declares
// ParentDimensionID: "cc").
func employeesAndCostCenters() map[string]*Dimension {
	return map[string]*Dimension{
		"cc": {
			ID: "cc",
			Members: []Member{
				{ID: "cc-ga", Code: "cc-ga"},
				{ID: "cc-sales", Code: "cc-sales"},
			},
		},
		"employees": {
			ID:                "employees",
			ParentDimensionID: "cc",
			Members: []Member{
				{ID: "emp-1", Code: "emp-1", ParentCode: "cc-ga"},
				{ID: "emp-2", Code: "emp-2", ParentCode: "cc-ga"},
				{ID: "emp-3", Code: "emp-3", ParentCode: "cc-sales"},
			},
		},
	}
}

func TestResolveStructuralChildChain(t *testing.T) {
	// Metric "salary" is dimensioned by [employees]; combo pins cost_centers
	// (a structural ancestor dimension of employees) to "cc-ga" — salary
	// must roll up emp-1 + emp-2, excluding emp-3.
	dims := employeesAndCostCenters()
	fetch := memFetch(t, map[string]float64{
		fetchKey("salary", map[string]string{"employees": "emp-1"}): 1000,
		fetchKey("salary", map[string]string{"employees": "emp-2"}): 2000,
		fetchKey("salary", map[string]string{"employees": "emp-3"}): 5000, // must not be included
	})

	v, ok, err := Resolve(context.Background(), dims, "salary", []string{"employees"}, AggSum,
		map[string]string{"cc": "cc-ga"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || v != 3000 {
		t.Errorf("salary rolled up under cc-ga = %v, ok=%v, want 3000, true", v, ok)
	}
}

// TestResolveStructuralChildChainWithZeroVisibleChildren is a regression
// test for a real bug caught during manual verification against live demo
// data: a cost center whose only employee is hidden (simply absent from
// Members, mirroring writeguard-filtered Dimension construction) must
// resolve to 0 — descendantsInChain legitimately returning zero codes was
// being misread by relate's nil-means-"no relation" check as "no relation
// exists at all", which then fell through to aggregating over EVERY OTHER
// cost center's employees instead of correctly resolving to zero.
func TestResolveStructuralChildChainWithZeroVisibleChildren(t *testing.T) {
	dims := employeesAndCostCenters()
	dims["cc"].Members = append(dims["cc"].Members, Member{ID: "cc-empty", Code: "cc-empty"})
	// No employees dimension member has ParentCode "cc-empty" at all.
	fetch := memFetch(t, map[string]float64{
		fetchKey("salary", map[string]string{"employees": "emp-1"}): 1000,
		fetchKey("salary", map[string]string{"employees": "emp-2"}): 2000,
		fetchKey("salary", map[string]string{"employees": "emp-3"}): 5000,
	})

	v, ok, err := Resolve(context.Background(), dims, "salary", []string{"employees"}, AggSum,
		map[string]string{"cc": "cc-empty"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || v != 0 {
		t.Errorf("salary under a cost center with zero visible employees = %v, ok=%v, want 0, true (not 8000 — the full-leaf-aggregate fallback bug)", v, ok)
	}
}

func TestResolveStructuralParentChain(t *testing.T) {
	// Metric "budget" is dimensioned by [cost_centers] (the parent
	// dimension); combo pins employees (a structural descendant) to
	// "emp-1" — budget must broadcast cc-ga's single value down.
	dims := employeesAndCostCenters()
	fetch := memFetch(t, map[string]float64{
		fetchKey("budget", map[string]string{"cc": "cc-ga"}): 9000,
	})

	v, ok, err := Resolve(context.Background(), dims, "budget", []string{"cc"}, AggSum,
		map[string]string{"employees": "emp-1"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || v != 9000 {
		t.Errorf("budget broadcast to emp-1 = %v, ok=%v, want 9000, true", v, ok)
	}
}

func TestResolvePropertyDerivedGrouping(t *testing.T) {
	// regions <- employees.properties["region"]: employees e1/e2 are
	// tagged region=east, e3 is tagged region=west. Metric "salary" is
	// dimensioned by [employees]; combo pins "regions" to "east".
	dims := map[string]*Dimension{
		"regions": {
			ID:                "regions",
			SourceDimensionID: "employees",
			SourceProperty:    "region",
			Members: []Member{
				{ID: "east", Code: "east"},
				{ID: "west", Code: "west"},
			},
		},
		"employees": {
			ID: "employees",
			Members: []Member{
				{ID: "e1", Code: "e1", Properties: map[string]string{"region": "east"}},
				{ID: "e2", Code: "e2", Properties: map[string]string{"region": "east"}},
				{ID: "e3", Code: "e3", Properties: map[string]string{"region": "west"}},
			},
		},
	}
	fetch := memFetch(t, map[string]float64{
		fetchKey("salary", map[string]string{"employees": "e1"}): 100,
		fetchKey("salary", map[string]string{"employees": "e2"}): 200,
		fetchKey("salary", map[string]string{"employees": "e3"}): 999, // must not be included
	})

	v, ok, err := Resolve(context.Background(), dims, "salary", []string{"employees"}, AggSum,
		map[string]string{"regions": "east"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || v != 300 {
		t.Errorf("salary grouped by region=east = %v, ok=%v, want 300, true", v, ok)
	}
}

func TestResolveUnrelatedAxisAggregatesOverAllLeaves(t *testing.T) {
	// Metric "revenue" is dimensioned by [months], but combo only pins
	// "dept" (unrelated to months entirely) — revenue must sum across
	// every month rather than fail to resolve.
	dims := map[string]*Dimension{
		"dept": deptDim(),
		"months": {
			ID: "months",
			Members: []Member{
				{ID: "jan", Code: "jan"},
				{ID: "feb", Code: "feb"},
			},
		},
	}
	fetch := memFetch(t, map[string]float64{
		fetchKey("revenue", map[string]string{"months": "jan"}): 10,
		fetchKey("revenue", map[string]string{"months": "feb"}): 20,
	})

	v, ok, err := Resolve(context.Background(), dims, "revenue", []string{"months"}, AggSum,
		map[string]string{"dept": "ga"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || v != 30 {
		t.Errorf("revenue aggregated over all months = %v, ok=%v, want 30, true", v, ok)
	}
}

func TestResolveMultiDimensionCartesianProduct(t *testing.T) {
	// Metric "salary" is dimensioned by [employees, months]; combo pins
	// cost_centers=cc-ga (structural, rolls up emp-1/emp-2) and leaves
	// months unpinned entirely (aggregated over all months) — the
	// Cartesian product must cover both employees x both months.
	dims := employeesAndCostCenters()
	dims["months"] = &Dimension{
		ID: "months",
		Members: []Member{
			{ID: "jan", Code: "jan"},
			{ID: "feb", Code: "feb"},
		},
	}
	table := map[string]float64{}
	for _, emp := range []string{"emp-1", "emp-2"} {
		for _, mo := range []string{"jan", "feb"} {
			table[fetchKey("salary", map[string]string{"employees": emp, "months": mo})] = 100
		}
	}
	// emp-3 (cc-sales) must never be reached.
	table[fetchKey("salary", map[string]string{"employees": "emp-3", "months": "jan"})] = 99999
	fetch := memFetch(t, table)

	v, ok, err := Resolve(context.Background(), dims, "salary", []string{"employees", "months"}, AggSum,
		map[string]string{"cc": "cc-ga"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || v != 400 {
		t.Errorf("salary over emp-1/emp-2 x jan/feb = %v, ok=%v, want 400, true", v, ok)
	}
}

func TestResolveZeroDimensionMetric(t *testing.T) {
	dims := map[string]*Dimension{}
	fetch := memFetch(t, map[string]float64{
		fetchKey("global_rate", map[string]string{}): 7,
	})

	v, ok, err := Resolve(context.Background(), dims, "global_rate", nil, AggSum,
		map[string]string{"dept": "ga"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || v != 7 {
		t.Errorf("zero-dimension metric = %v, ok=%v, want 7, true", v, ok)
	}
}

func TestResolveDepthExceededOnCyclicHierarchy(t *testing.T) {
	// A malformed member hierarchy where a member is its own ancestor.
	dims := map[string]*Dimension{"dept": {
		ID: "dept",
		Members: []Member{
			{ID: "a", Code: "a", ParentCode: "b"},
			{ID: "b", Code: "b", ParentCode: "a"},
		},
	}}
	fetch := memFetch(t, map[string]float64{})

	_, _, err := Resolve(context.Background(), dims, "amount", []string{"dept"}, AggSum,
		map[string]string{"dept": "a"}, fetch)
	if !errors.Is(err, ErrDepthExceeded) {
		t.Errorf("expected ErrDepthExceeded on a cyclic hierarchy, got %v", err)
	}
}

func TestResolveLegitimatelyDeepHierarchyDoesNotFalsePositive(t *testing.T) {
	const chainLen = 8 // under maxDepth
	dim := &Dimension{ID: "dept"}
	prev := ""
	for i := 0; i < chainLen; i++ {
		code := string(rune('a' + i))
		dim.Members = append(dim.Members, Member{ID: code, Code: code, ParentCode: prev})
		prev = code
	}
	dims := map[string]*Dimension{"dept": dim}
	leaf := prev
	fetch := memFetch(t, map[string]float64{
		fetchKey("amount", map[string]string{"dept": leaf}): 5,
	})

	v, ok, err := Resolve(context.Background(), dims, "amount", []string{"dept"}, AggSum,
		map[string]string{"dept": "a"}, fetch)
	if err != nil {
		t.Fatalf("unexpected error on a legitimately deep (but under-cap) hierarchy: %v", err)
	}
	if !ok || v != 5 {
		t.Errorf("deep chain rollup = %v, ok=%v, want 5, true", v, ok)
	}
}

func TestLeafCombosSingleDimension(t *testing.T) {
	dims := map[string]*Dimension{"dept": deptDim()}
	combos := LeafCombos(dims, []string{"dept"})

	got := make(map[string]bool, len(combos))
	for _, c := range combos {
		got[c["dept"]] = true
	}
	want := map[string]bool{"ga": true, "sales": true}
	if len(got) != len(want) {
		t.Fatalf("LeafCombos = %v, want exactly %v (region-a is a rollup parent, not a leaf)", combos, want)
	}
	for code := range want {
		if !got[code] {
			t.Errorf("LeafCombos missing leaf %q, got %v", code, combos)
		}
	}
}

func TestLeafCombosMultiDimension(t *testing.T) {
	dims := employeesAndCostCenters()
	dims["months"] = &Dimension{
		ID: "months",
		Members: []Member{
			{ID: "jan", Code: "jan"},
			{ID: "feb", Code: "feb"},
		},
	}
	combos := LeafCombos(dims, []string{"employees", "months"})
	// 3 employees (emp-1, emp-2, emp-3) x 2 months = 6 combos.
	if len(combos) != 6 {
		t.Fatalf("LeafCombos count = %d, want 6, got %v", len(combos), combos)
	}
	seen := map[string]bool{}
	for _, c := range combos {
		if len(c) != 2 {
			t.Fatalf("combo %v does not pin both dimensions", c)
		}
		seen[c["employees"]+"|"+c["months"]] = true
	}
	for _, emp := range []string{"emp-1", "emp-2", "emp-3"} {
		for _, mo := range []string{"jan", "feb"} {
			if !seen[emp+"|"+mo] {
				t.Errorf("LeafCombos missing combo %s|%s, got %v", emp, mo, combos)
			}
		}
	}
}

func TestLeafCombosEmptyDimIDs(t *testing.T) {
	dims := map[string]*Dimension{"dept": deptDim()}
	combos := LeafCombos(dims, nil)
	if len(combos) != 1 || len(combos[0]) != 0 {
		t.Errorf("LeafCombos(nil dimIDs) = %v, want a single empty combo", combos)
	}
}

func TestLeafCombosZeroLeafMembersCollapsesToNil(t *testing.T) {
	// A dimension with a declared entry but literally no members (e.g. every
	// member currently hidden) must collapse the WHOLE product to nil, not
	// just omit that one axis — mirrors cartesianProduct's existing
	// empty-set-collapses-the-whole-product behavior, which
	// executePartition relies on to fall back to a single {} combo.
	dims := map[string]*Dimension{
		"dept":  deptDim(),
		"empty": {ID: "empty"},
	}
	combos := LeafCombos(dims, []string{"dept", "empty"})
	if combos != nil {
		t.Errorf("LeafCombos with one zero-member dimension = %v, want nil", combos)
	}
}
