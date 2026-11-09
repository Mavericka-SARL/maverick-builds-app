package calculation_test

import (
	"context"
	"testing"
)

// TestLookupOfASourceWithNoValueByItsRules: a source with time summary
// none has no value at an aggregate period or over every period, and one
// with aggregation none has none above its members. LOOKUP there is blank,
// as YEARVALUE is: no row is stored and ISBLANK is TRUE. It persisted a row
// of 0 before. A member with nothing recorded still reads 0.
func TestLookupOfASourceWithNoValueByItsRules(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000f6"
	revID := "00000000-0000-0000-0001-0000000000f6"

	monthID, months := monthHierarchy(t, store, ctx, modelID, revID, 1)
	regionID := insertDimRev(t, store, ctx, modelID, revID, "region")
	world := addMember(t, store, ctx, regionID, "World", "", nil)
	addMember(t, store, ctx, regionID, "EMEA", world, nil)
	addMember(t, store, ctx, regionID, "US", world, nil)

	bnID := insertMetric(t, store, ctx, modelID, revID, "bn", "", true)
	setMetricField(t, store, ctx, bnID, "time_summary", "none")
	xnID := insertMetric(t, store, ctx, modelID, revID, "xn", "", true)
	setMetricField(t, store, ctx, xnID, "agg_rule", "none")

	formulas := map[string]string{
		"lkn":  `LOOKUP(bn, month, "FY26")`,
		"lknq": `LOOKUP(bn, month, "Q1")`,
		"isbn": `IF(ISBLANK(LOOKUP(bn, month, "FY26")), 1, 2)`,
		"lknl": `LOOKUP(bn, month, "2026-12")`, // a leaf with nothing recorded: 0, as before
		"lkx":  `LOOKUP(xn, region, "World")`,
		"lkxl": `LOOKUP(xn, region, "US")`,
	}
	ids := map[string]string{}
	labels := map[string]string{}
	for name, f := range formulas {
		id := insertCalc(t, store, ctx, modelID, revID, name, f)
		ids[name] = id
		labels[id] = name
	}
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, []string{bnID, ids["lkn"], ids["lknq"], ids["isbn"], ids["lknl"]})
	insertGridSetup(t, store, ctx, modelID, []string{regionID}, []string{xnID})
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, []string{ids["lkx"], ids["lkxl"]})
	for i := 0; i < 3; i++ {
		insertFact(t, store, ctx, modelID, revID, bnID, factJSON(monthID, months[i]), float64(i+1))
	}
	insertFact(t, store, ctx, modelID, revID, xnID, factJSON(regionID, "EMEA"), 4)
	insertFact(t, store, ctx, modelID, revID, xnID, factJSON(regionID, "US"), 6)
	runRecalc(t, store, ctx, modelID, revID, []string{bnID, xnID})

	c := rowChecker{t, store, ctx, modelID, revID, labels}
	at := func(m string) map[string]string { return pins(monthID, m) }
	for _, name := range []string{"lkn", "lknq", "lkx"} {
		for _, m := range []string{"2026-01", "2026-03", "2026-11"} {
			if v, ok := calcRow(t, store, ctx, modelID, revID, ids[name], at(m)); ok {
				t.Errorf("%s @ %s: no value by the source's rules must persist no row, got %v", name, m, v)
			}
		}
	}
	c.want(ids["isbn"], at("2026-01"), 1)
	c.want(ids["lknl"], at("2026-01"), 0)
	c.want(ids["lkxl"], at("2026-01"), 6)
	for _, name := range []string{"lkn", "lknq", "isbn", "lknl", "lkx", "lkxl"} {
		if status, msg := partitionState(t, store, ctx, ids[name]); status != "clean" {
			t.Errorf("%s: partition %s %q", name, status, msg)
		}
	}
}
