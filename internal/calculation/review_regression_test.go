package calculation_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// Regressions from the final review of the dimensional-references change.

// TestFormulaSourceReadAtPartialCoordinates: LOOKUP (and a plain
// reference) of an agg_rule=formula source from a cell pinning only some of
// the source's dimensions evaluates the source's formula there — revenue
// over units summed across the channels — never the mean of the persisted
// children's ratios (no row exists at such a combination).
func TestFormulaSourceReadAtPartialCoordinates(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000f1"
	revID := "00000000-0000-0000-0001-0000000000f1"

	regionID := regionFixture(t, store, ctx, modelID, revID)
	productID := insertDimRev(t, store, ctx, modelID, revID, "product")
	addMember(t, store, ctx, productID, "P1", "", nil)
	addMember(t, store, ctx, productID, "P2", "", nil)
	channelID := insertDimRev(t, store, ctx, modelID, revID, "channel")
	addMember(t, store, ctx, channelID, "Online", "", nil)
	addMember(t, store, ctx, channelID, "Retail", "", nil)

	revenueID := insertMetric(t, store, ctx, modelID, revID, "revenue", "", true)
	unitsID := insertMetric(t, store, ctx, modelID, revID, "units", "", true)
	priceID := insertCalc(t, store, ctx, modelID, revID, "price", "revenue / units")
	setMetricField(t, store, ctx, priceID, "agg_rule", "formula")
	worldID := insertCalc(t, store, ctx, modelID, revID, "world_price", `LOOKUP(price, region, "World")`)
	setMetricField(t, store, ctx, worldID, "agg_rule", "formula")
	plainID := insertCalc(t, store, ctx, modelID, revID, "plain_price", "price")
	setMetricField(t, store, ctx, plainID, "agg_rule", "formula")
	insertGridSetup(t, store, ctx, modelID, []string{regionID, productID, channelID}, []string{revenueID, unitsID, priceID})
	insertGridSetup(t, store, ctx, modelID, []string{regionID, productID}, []string{worldID, plainID})

	type cell struct{ region, channel string }
	rev := map[cell]float64{{"DE", "Online"}: 600, {"DE", "Retail"}: 400, {"FR", "Online"}: 300, {"US", "Retail"}: 50}
	units := map[cell]float64{{"DE", "Online"}: 50, {"DE", "Retail"}: 51, {"FR", "Online"}: 4, {"US", "Retail"}: 4}
	for k, v := range rev {
		insertFact(t, store, ctx, modelID, revID, revenueID, factJSON(regionID, k.region, productID, "P1", channelID, k.channel), v)
		insertFact(t, store, ctx, modelID, revID, unitsID, factJSON(regionID, k.region, productID, "P1", channelID, k.channel), units[k])
	}
	runRecalc(t, store, ctx, modelID, revID, []string{revenueID, unitsID})

	c := rowChecker{t, store, ctx, modelID, revID, map[string]string{priceID: "price", worldID: "world_price", plainID: "plain_price"}}
	rp := func(region string) map[string]string { return pins(regionID, region, productID, "P1") }
	c.want(priceID, pins(regionID, "World", productID, "P1", channelID, "Online"), 900.0/54)
	c.want(worldID, rp("DE"), 1350.0/109)
	c.want(worldID, rp("US"), 1350.0/109)
	c.want(plainID, rp("DE"), 1000.0/101)
	c.want(plainID, rp("FR"), 300.0/4)
	c.want(plainID, rp("EMEA"), 1300.0/105)
	for name, id := range map[string]string{"world_price": worldID, "plain_price": plainID} {
		if status, msg := partitionState(t, store, ctx, id); status != "clean" {
			t.Errorf("%s: partition %s %q", name, status, msg)
		}
	}
}

// TestYearValueAtParentSkipsEmptyPeriods: YEARVALUE and a LOOKUP to an
// aggregate period at a PARENT member skip the periods no leaf beneath
// recorded, as they do at a leaf — the closing balance of EMEA is DE's
// March balance, and the average is the mean of the recorded months, never
// a reduction over zeros.
func TestYearValueAtParentSkipsEmptyPeriods(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000f2"
	revID := "00000000-0000-0000-0001-0000000000f2"

	regionID := regionFixture(t, store, ctx, modelID, revID)
	monthID, _ := monthHierarchy(t, store, ctx, modelID, revID, 1)
	balID := insertMetric(t, store, ctx, modelID, revID, "bal", "", true)
	setMetricField(t, store, ctx, balID, "time_summary", "last")
	avgID := insertMetric(t, store, ctx, modelID, revID, "avgp", "", true)
	setMetricField(t, store, ctx, avgID, "time_summary", "average")
	ids := map[string]string{}
	labels := map[string]string{}
	all := []string{balID, avgID}
	for name, f := range map[string]string{
		"yvl": "YEARVALUE(bal)",
		"yva": "YEARVALUE(avgp)",
		"fyl": `LOOKUP(bal, month, "FY26")`,
	} {
		id := insertCalc(t, store, ctx, modelID, revID, name, f)
		setMetricField(t, store, ctx, id, "agg_rule", "formula")
		ids[name], labels[id] = id, name
		all = append(all, id)
	}
	insertGridSetup(t, store, ctx, modelID, []string{regionID, monthID}, all)
	for i, m := range []string{"2026-01", "2026-02", "2026-03"} {
		insertFact(t, store, ctx, modelID, revID, balID, factJSON(regionID, "DE", monthID, m), float64(10*(i+1)))
		insertFact(t, store, ctx, modelID, revID, avgID, factJSON(regionID, "DE", monthID, m), float64(10*(i+1)))
	}
	runRecalc(t, store, ctx, modelID, revID, []string{balID, avgID})

	c := rowChecker{t, store, ctx, modelID, revID, labels}
	at := func(region, m string) map[string]string { return pins(regionID, region, monthID, m) }
	c.want(ids["yvl"], at("DE", "2026-01"), 30)
	c.want(ids["yvl"], at("EMEA", "2026-01"), 30)
	c.want(ids["yvl"], at("World", "2026-05"), 30)
	c.want(ids["yva"], at("DE", "2026-01"), 20)
	c.want(ids["yva"], at("EMEA", "2026-01"), 20)
	c.want(ids["fyl"], at("DE", "2026-05"), 30)
	c.want(ids["fyl"], at("EMEA", "2026-05"), 30)
	for name, id := range ids {
		if status, msg := partitionState(t, store, ctx, id); status != "clean" {
			t.Errorf("%s: partition %s %q", name, status, msg)
		}
	}
}

// TestGroupingDimensionRecalcOnMemberChange: SUMIFS along a property
// grouping (area groups employees by their area property) is recomputed
// when an employee's area changes — RecalcDimensionDependents of the
// grouped dimension reaches formulas naming the grouping dimension.
func TestGroupingDimensionRecalcOnMemberChange(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000f3"
	revID := "00000000-0000-0000-0001-0000000000f3"

	empID := insertDimRev(t, store, ctx, modelID, revID, "employees")
	declareProperty(t, store, ctx, empID, "area", "text")
	e2 := ""
	for _, e := range []struct{ code, area string }{{"E1", "EMEA"}, {"E2", "AMER"}, {"E3", "EMEA"}} {
		id := addMember(t, store, ctx, empID, e.code, "", map[string]string{"area": e.area})
		if e.code == "E2" {
			e2 = id
		}
	}
	areaID := insertDimRev(t, store, ctx, modelID, revID, "area")
	if _, err := store.Pool().Exec(ctx, `UPDATE model.dimension_def SET source_dimension_id=$2::uuid, source_property='area' WHERE id=$1::uuid`, areaID, empID); err != nil {
		t.Fatal(err)
	}
	addMember(t, store, ctx, areaID, "EMEA", "", nil)
	addMember(t, store, ctx, areaID, "AMER", "", nil)

	salaryID := insertMetric(t, store, ctx, modelID, revID, "salary", "", true)
	emeaID := insertCalc(t, store, ctx, modelID, revID, "emea_salary", `SUMIFS(salary, area, "EMEA")`)
	insertGridSetup(t, store, ctx, modelID, []string{empID}, []string{salaryID, emeaID})
	for code, v := range map[string]float64{"E1": 100, "E2": 200, "E3": 50} {
		insertFact(t, store, ctx, modelID, revID, salaryID, factJSON(empID, code), v)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{salaryID})
	c := rowChecker{t, store, ctx, modelID, revID, map[string]string{emeaID: "emea_salary"}}
	c.want(emeaID, pins(empID, "E1"), 150)

	if _, err := store.Pool().Exec(ctx, `UPDATE model.dimension_member SET properties='{"area":"EMEA"}'::jsonb WHERE id=$1::uuid`, e2); err != nil {
		t.Fatal(err)
	}
	sched := calculation.NewScheduler(logger.New("calc-test"), store, nil)
	if err := sched.RecalcDimensionDependents(ctx, empID); err != nil {
		t.Fatalf("RecalcDimensionDependents: %v", err)
	}
	c.want(emeaID, pins(empID, "E1"), 350)
}

// TestLookupOfDeletedMemberClearsRows: once the member a literal LOOKUP
// names is deleted, every cell fails with #N/A — and the rows computed
// from that member's data go, rather than being served as current.
func TestLookupOfDeletedMemberClearsRows(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000f4"
	revID := "00000000-0000-0000-0001-0000000000f4"

	regionID := regionFixture(t, store, ctx, modelID, revID)
	salesID := insertMetric(t, store, ctx, modelID, revID, "sales", "", true)
	lkID := insertCalc(t, store, ctx, modelID, revID, "lk_us", `LOOKUP(sales, region, "US")`)
	insertGridSetup(t, store, ctx, modelID, []string{regionID}, []string{salesID, lkID})
	insertFact(t, store, ctx, modelID, revID, salesID, factJSON(regionID, "DE"), 10)
	insertFact(t, store, ctx, modelID, revID, salesID, factJSON(regionID, "US"), 500)
	runRecalc(t, store, ctx, modelID, revID, []string{salesID})
	c := rowChecker{t, store, ctx, modelID, revID, map[string]string{lkID: "lk_us"}}
	c.want(lkID, pins(regionID, "DE"), 500)

	usID := memberID(t, store, ctx, regionID, "US")
	if _, err := store.Pool().Exec(ctx, `DELETE FROM runtime.fact_input WHERE metric_id=$1::uuid AND dim_members->>$2 = 'US'`, salesID, regionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `DELETE FROM model.dimension_member WHERE id=$1::uuid`, usID); err != nil {
		t.Fatal(err)
	}
	sched := calculation.NewScheduler(logger.New("calc-test"), store, nil)
	_ = sched.RecalcAffected(ctx, modelID, revID, []string{salesID})
	_ = sched.RecalcDimensionDependents(ctx, regionID)

	for _, dims := range []map[string]string{pins(regionID, "DE"), nil} {
		if v, ok := calcRow(t, store, ctx, modelID, revID, lkID, dims); ok {
			t.Errorf("lk_us @ %v = %v after US was deleted, want no row", dims, v)
		}
	}
	if status, msg := partitionState(t, store, ctx, lkID); status != "error" || !strings.Contains(msg, "#N/A") || !strings.Contains(msg, "US") {
		t.Errorf("partition: %s %q, want an error naming the missing member", status, msg)
	}
}
