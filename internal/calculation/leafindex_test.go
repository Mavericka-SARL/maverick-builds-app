package calculation

import (
	"math/rand"
	"slices"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// The leaf index finds, for every coordinate a time-series metric's totals
// read, exactly the leaves a scan of all of them found — the same rows in
// the same order, so the totals add up in the same sequence: under a
// parent member, under two pins at once, past a leaf at no period of the
// axis and one missing a dimension.
func TestLeafIndexFindsWhatAScanFinds(t *testing.T) {
	dims := map[string]*rollup.Dimension{
		"region": {ID: "region", Members: []rollup.Member{
			{Code: "EU"}, {Code: "FR", ParentCode: "EU"}, {Code: "DE", ParentCode: "EU"}, {Code: "PAR", ParentCode: "FR"}, {Code: "US"},
		}},
		"product": {ID: "product", Members: []rollup.Member{{Code: "P1"}, {Code: "P2"}}},
	}
	axis := &timeAxis{dim: &rollup.Dimension{ID: "t", IsTime: true}, index: map[string]int{}}
	for i, code := range []string{"D1", "D2", "D3", "D4"} {
		axis.periods = append(axis.periods, formula.TimePeriod{Code: code, Index: i})
		axis.index[code] = i
	}
	rnd := rand.New(rand.NewSource(7))
	var leaf []CalcResultRow
	for _, day := range []string{"D1", "D2", "D3", "D4", "D9"} { // D9: at no period
		for _, region := range []string{"FR", "DE", "PAR", "US", "EU"} {
			for _, product := range []string{"P1", "P2"} {
				if rnd.Intn(3) == 0 {
					continue
				}
				leaf = append(leaf, CalcResultRow{DimMembers: map[string]string{"t": day, "region": region, "product": product}, Value: rnd.Float64()})
			}
		}
	}
	leaf = append(leaf, CalcResultRow{DimMembers: map[string]string{"t": "D2", "region": "FR"}, Value: 1}) // no product
	rnd.Shuffle(len(leaf), func(i, j int) { leaf[i], leaf[j] = leaf[j], leaf[i] })

	idx := newLeafIndex(leaf, axis, []string{"region", "t", "product"})
	sub := func(dimID, code string) map[string]bool { return subtreeCodes(dims[dimID], code) }
	scan := func(pins map[string]string, pos int) []int {
		var out []int
		for i, r := range leaf {
			if r.DimMembers["t"] != axis.periods[pos].Code {
				continue
			}
			match := true
			for dimID, code := range pins {
				if !sub(dimID, code)[r.DimMembers[dimID]] {
					match = false
				}
			}
			if match {
				out = append(out, i)
			}
		}
		return out
	}
	for _, pins := range []map[string]string{
		{}, {"region": "EU"}, {"region": "FR"}, {"region": "PAR"}, {"region": "US"}, {"region": "XX"},
		{"product": "P2"}, {"region": "EU", "product": "P1"}, {"region": "FR", "product": "P2"},
	} {
		for pos := range axis.periods {
			want, got := scan(pins, pos), idx.rows(pins, pos, sub)
			if len(want) == 0 && len(got) == 0 {
				continue
			}
			if !slices.Equal(got, want) {
				t.Errorf("pins %v at %s: index found %v, a scan %v", pins, axis.periods[pos].Code, got, want)
			}
		}
	}
}

// A slice under a parent member adds its children's leaves in the order
// they are stored, as the scan of every leaf did: with 1e16 and 1 among
// them, adding them child by child gives another total.
func TestSliceTotalsAddLeavesInTheirOrder(t *testing.T) {
	dims := map[string]*rollup.Dimension{
		"region":  {ID: "region", Members: []rollup.Member{{Code: "EU"}, {Code: "FR", ParentCode: "EU"}, {Code: "DE", ParentCode: "EU"}}},
		"product": {ID: "product", Members: []rollup.Member{{Code: "P1"}}},
	}
	leaf := []CalcResultRow{
		{DimMembers: map[string]string{"region": "FR", "product": "P1"}, Value: 1e16},
		{DimMembers: map[string]string{"region": "DE", "product": "P1"}, Value: 1},
		{DimMembers: map[string]string{"region": "FR", "product": "P1"}, Value: -1e16},
		{DimMembers: map[string]string{"region": "DE", "product": "P1"}, Value: 1},
	}
	var inOrder float64
	for _, r := range leaf {
		inOrder += r.Value
	}
	rows := oneDimSliceRows("sum", false, []string{"region", "product"}, dims, leaf, nil, nil, "")
	found := false
	for _, r := range rows {
		if r.DimMembers["region"] == "EU" {
			found = true
			if r.Value != inOrder {
				t.Fatalf("EU = %v, want %v (the leaves added in their order)", r.Value, inOrder)
			}
		}
	}
	if !found {
		t.Fatal("no EU slice row")
	}
}
