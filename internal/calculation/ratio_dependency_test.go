package calculation_test

import (
	"context"
	"testing"
)

// TestPlainReadOfAPureRatioAboveItsLeaves: a pure ratio with agg_rule
// average totals as its formula (EffectiveAggRule), and so does every read
// of it above its leaves. x = margin * 2 on [region] read margin at EMEA
// (product unpinned) as the mean of its three products' margins (0.2333,
// x = 0.4667), while margin's own EMEA row — and the grid's scoped read —
// is the ratio of the sums, 40 / 200.
func TestPlainReadOfAPureRatioAboveItsLeaves(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000f3"
	revID := "00000000-0000-0000-0001-0000000000f3"

	regionID := insertDimRev(t, store, ctx, modelID, revID, "region")
	world := addMember(t, store, ctx, regionID, "World", "", nil)
	addMember(t, store, ctx, regionID, "EMEA", world, nil)
	addMember(t, store, ctx, regionID, "US", world, nil)
	productID := insertDimRev(t, store, ctx, modelID, revID, "product")
	for _, p := range []string{"P1", "P2", "P3"} {
		addMember(t, store, ctx, productID, p, "", nil)
	}
	profitID := insertMetric(t, store, ctx, modelID, revID, "profit", "", true)
	revenueID := insertMetric(t, store, ctx, modelID, revID, "revenue", "", true)
	marginID := insertCalc(t, store, ctx, modelID, revID, "margin", "profit / revenue")
	setMetricField(t, store, ctx, marginID, "agg_rule", "average")
	xID := insertCalc(t, store, ctx, modelID, revID, "x", "margin * 2")
	lkID := insertCalc(t, store, ctx, modelID, revID, "lk", `LOOKUP(margin, region, "EMEA")`)
	insertGridSetup(t, store, ctx, modelID, []string{regionID, productID}, []string{profitID, revenueID, marginID})
	insertGridSetup(t, store, ctx, modelID, []string{regionID}, []string{xID})
	insertGridSetup(t, store, ctx, modelID, nil, []string{lkID})

	for _, f := range []struct {
		region, product string
		profit, revenue float64
	}{{"EMEA", "P1", 10, 100}, {"EMEA", "P2", 30, 50}, {"EMEA", "P3", 0, 50}, {"US", "P1", 5, 10}} {
		insertFact(t, store, ctx, modelID, revID, profitID, factJSON(regionID, f.region, productID, f.product), f.profit)
		insertFact(t, store, ctx, modelID, revID, revenueID, factJSON(regionID, f.region, productID, f.product), f.revenue)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{profitID, revenueID})

	c := rowChecker{t, store, ctx, modelID, revID, map[string]string{marginID: "margin", xID: "x", lkID: "lk"}}
	c.want(marginID, pins(regionID, "EMEA"), 0.2) // its own slice row: 40 / 200
	c.want(xID, pins(regionID, "EMEA"), 0.4)      // 2 × that, not 2 × mean(0.1, 0.6, 0)
	c.want(xID, pins(regionID, "US"), 1)
	// LOOKUP from a grid without product reads margin at EMEA over every
	// product: its EMEA row again.
	c.want(lkID, map[string]string{}, 0.2)
	for id, name := range map[string]string{xID: "x", lkID: "lk"} {
		if status, msg := partitionState(t, store, ctx, id); status != "clean" {
			t.Errorf("%s: partition %s %q", name, status, msg)
		}
	}
}

// TestTimeSeriesPureRatioReadAboveItsLeaves: the same for a time-series
// pure ratio. Its rollup rows (World × Feb) are written as its formula
// there, as for agg_rule formula, so a LOOKUP of World reads them instead
// of failing ("a time-series formula is not evaluated on demand"), and a
// [month] reader gets the ratio of the sums, not the mean of the regions.
func TestTimeSeriesPureRatioReadAboveItsLeaves(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000f4"
	revID := "00000000-0000-0000-0001-0000000000f4"

	regionID := insertDimRev(t, store, ctx, modelID, revID, "region")
	world := addMember(t, store, ctx, regionID, "World", "", nil)
	addMember(t, store, ctx, regionID, "EMEA", world, nil)
	addMember(t, store, ctx, regionID, "US", world, nil)
	monthID := insertTimeDim(t, store, ctx, modelID, revID, "month")
	months := insertMonths(t, store, ctx, monthID, 3)

	profitID := insertMetric(t, store, ctx, modelID, revID, "profit", "", true)
	revenueID := insertMetric(t, store, ctx, modelID, revID, "revenue", "", true)
	marginID := insertCalc(t, store, ctx, modelID, revID, "margin2", "PREVIOUS(profit) / revenue")
	setMetricField(t, store, ctx, marginID, "agg_rule", "average")
	yID := insertCalc(t, store, ctx, modelID, revID, "y", "margin2 * 2")
	zID := insertCalc(t, store, ctx, modelID, revID, "z", `LOOKUP(margin2, region, "World")`)
	insertGridSetup(t, store, ctx, modelID, []string{regionID, monthID}, []string{profitID, revenueID, marginID})
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, []string{yID, zID})

	fact := func(id, region string, month int, v float64) {
		insertFact(t, store, ctx, modelID, revID, id, factJSON(regionID, region, monthID, months[month]), v)
	}
	fact(profitID, "EMEA", 0, 10)
	fact(profitID, "US", 0, 5)
	fact(profitID, "EMEA", 1, 20)
	fact(profitID, "US", 1, 5)
	for m := 0; m < 3; m++ {
		fact(revenueID, "EMEA", m, 100)
		fact(revenueID, "US", m, 10)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{profitID, revenueID})

	c := rowChecker{t, store, ctx, modelID, revID, map[string]string{marginID: "margin2", yID: "y", zID: "z"}}
	feb := months[1]
	c.want(marginID, pins(regionID, "EMEA", monthID, feb), 0.1)
	c.want(marginID, pins(regionID, "US", monthID, feb), 0.5)
	c.want(marginID, pins(regionID, "World", monthID, feb), 15.0/110) // its rollup row: the formula at World
	c.want(yID, pins(monthID, feb), 2*15.0/110)                       // not 2 × mean(0.1, 0.5)
	c.want(zID, pins(monthID, feb), 15.0/110)
	c.want(yID, pins(monthID, months[2]), 2*25.0/110)
	for id, name := range map[string]string{marginID: "margin2", yID: "y", zID: "z"} {
		if status, msg := partitionState(t, store, ctx, id); status != "clean" {
			t.Errorf("%s: partition %s %q", name, status, msg)
		}
	}
}
