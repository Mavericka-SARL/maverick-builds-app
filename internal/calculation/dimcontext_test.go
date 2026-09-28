package calculation_test

import (
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// The exported cell-local builder the gateway and chart recomputes use:
// dim.property and PARENT evaluate from metadata alone, while LOOKUP and
// the conditional aggregations over a source can never read a silent 0.
func TestCellContextIsCellLocal(t *testing.T) {
	dims := map[string]*rollup.Dimension{
		"d-region": {ID: "d-region", Members: []rollup.Member{
			{Code: "World", TimeIndex: -1},
			{Code: "DE", ParentCode: "World", Properties: map[string]string{"weight": "2", "segment": "Ent"}},
			{Code: "FR", ParentCode: "World", Properties: map[string]string{"weight": "x", "segment": "SMB"}},
		}},
	}
	names := map[string]string{"d-region": "region"}
	schema := &calculation.DimensionSchema{Properties: map[string]map[string]calculation.PropertyDecl{
		"d-region": {"WEIGHT": {Name: "weight", DataType: "number"}, "SEGMENT": {Name: "segment", DataType: "text"}},
	}}
	meta := calculation.NewDimMetadata(dims, names, schema)
	de := map[string]string{"d-region": "DE"}

	eval := func(expr string, combo map[string]string) (float64, error) {
		return calculation.EvaluateWithDimContext(expr, map[string]float64{"revenue": 10}, map[string]string{"region": combo["d-region"]}, meta.CellContext(combo))
	}
	cases := []struct {
		expr string
		want float64
	}{
		{"revenue * REGION.Weight", 20},
		{`IF(PARENT(region) = "World", 1, 0)`, 1},
		{`IF(region.segment = "Ent", 1, 0)`, 1},
		{`COUNTIFS(region.segment, "Ent")`, 1},
	}
	for _, c := range cases {
		got, err := eval(c.expr, de)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v; want %v", c.expr, got, err, c.want)
		}
	}
	// Not pinned: blank, never the member of another cell.
	if got, err := eval(`IF(region.segment = "", 1, 0)`, map[string]string{}); err != nil || got != 1 {
		t.Errorf("unpinned property must be blank: %v %v", got, err)
	}
	for expr, code := range map[string]string{
		`LOOKUP(revenue, region, "DE")`:   formula.CodeDimContextRequired,
		`SUMIFS(revenue, region, "*")`:    formula.CodeDimContextRequired,
		"revenue * region.nope":           formula.CodeUnknownProperty,
		"revenue * nowhere.weight":        "#NAME?",
		`LOOKUP(revenue, region, "ZZ")`:   "#N/A",
		"revenue * region.weight + 0 * 1": "",
	} {
		_, err := eval(expr, de)
		if code == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", expr, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), code) {
			t.Errorf("%s: want %s, got %v", expr, code, err)
		}
	}
	// An unparsable number property is #VALUE!, not 0.
	if _, err := eval("revenue * region.weight", map[string]string{"d-region": "FR"}); err == nil || !strings.Contains(err.Error(), "#VALUE!") {
		t.Errorf("unparsable number property: want #VALUE!, got %v", err)
	}
}

// FormulaReferencesDims matches dimension names case-insensitively (C1) and
// treats dim.property, PARENT, LOOKUP and the conditional aggregations as
// dimension-conditional.
func TestFormulaReferencesDimsCaseInsensitive(t *testing.T) {
	names := map[string]string{"d1": "region"}
	for expr, want := range map[string]bool{
		"revenue * region.weight":                  true,
		"revenue * REGION.weight":                  true,
		`IF(Region = "DE", revenue, 0)`:            true,
		`IF(PARENT(REGION) = "EMEA", revenue, 0)`:  true,
		`revenue / LOOKUP(revenue, product, "P1")`: true,
		`SUMIFS(revenue, product, "P1")`:           true,
		"revenue / cost":                           false,
		"revenue +":                                false, // does not parse: word scan finds no dimension
		"regional_revenue * 2":                     false,
	} {
		if got := calculation.FormulaReferencesDims(expr, names); got != want {
			t.Errorf("FormulaReferencesDims(%q) = %v, want %v", expr, got, want)
		}
	}
}

// Where a metric shares a dimension's name, the evaluators bind a pinned
// dimension's member code after the metric values, so the bare name is the
// member code at a pinned cell and the metric only where the dimension is
// unpinned. This pins what FORMULA_CALCULATION_INSTRUCTIONS.md documents.
func TestDimensionNameSharedWithMetric(t *testing.T) {
	dims := map[string]*rollup.Dimension{
		"d-region": {ID: "d-region", Members: []rollup.Member{
			{Code: "World", TimeIndex: -1},
			{Code: "DE", ParentCode: "World"},
		}},
	}
	names := map[string]string{"d-region": "region"}
	meta := calculation.NewDimMetadata(dims, names, &calculation.DimensionSchema{})
	values := map[string]float64{"region": 3}

	// Unpinned: the metric named region is read.
	got, err := calculation.EvaluateWithDimContext("region * 2", values, nil, meta.CellContext(map[string]string{}))
	if err != nil || got != 6 {
		t.Errorf("unpinned: want metric 3*2=6, got %v, %v", got, err)
	}

	// Pinned: the member code overwrites the metric.
	pinned := map[string]string{"d-region": "DE"}
	got, err = calculation.EvaluateWithDimContext(`IF(region = "DE", 1, 0)`, values, map[string]string{"region": "DE"}, meta.CellContext(pinned))
	if err != nil || got != 1 {
		t.Errorf("pinned: want the member code DE (1), got %v, %v", got, err)
	}
	if _, err := calculation.EvaluateWithDimContext("region * 2", values, map[string]string{"region": "DE"}, meta.CellContext(pinned)); err == nil || !strings.Contains(err.Error(), "#VALUE!") {
		t.Errorf("pinned: arithmetic on the member code must be #VALUE!, got %v", err)
	}
}
