package calculation_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
)

// Regressions found by verifying stage 2a of the dimensional-references
// contract (FORMULA_CALCULATION_INSTRUCTIONS.md, C1-C5) against a live
// database.

func memberID(t *testing.T, store *calculation.Store, ctx context.Context, dimID, code string) string {
	t.Helper()
	var id string
	if err := store.Pool().QueryRow(ctx,
		`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, dimID, code).Scan(&id); err != nil {
		t.Fatalf("member %s: %v", code, err)
	}
	return id
}

// TestDimensionalRegressionsScalar:
//   - an unknown member reached through a property fails the partition even
//     when the formula is nothing but the LOOKUP (nothing read before the
//     member check is not "no data");
//   - dimension names match case-insensitively when deciding whether an
//     agg_rule=average formula is dimension-conditional (REGION.weight);
//   - LOOKUP and plain references of an agg_rule=formula source above the
//     leaves read the source's persisted value (the formula at that
//     coordinate), never a mean of its children;
//   - a blank result persists no row, never a 0;
//   - once all data is deleted, the '{}' total goes with the per-combo rows.
func TestDimensionalRegressionsScalar(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000e1"
	revID := "00000000-0000-0000-0001-0000000000e1"

	regionID := regionFixture(t, store, ctx, modelID, revID)
	// UK: a leaf whose currency names a member the currency dimension does
	// not have, and with no weight.
	addMember(t, store, ctx, regionID, "UK", memberID(t, store, ctx, regionID, "EMEA"), map[string]string{"Currency": "GBP"})
	productID := insertDimRev(t, store, ctx, modelID, revID, "product")
	addMember(t, store, ctx, productID, "P1", "", nil)
	addMember(t, store, ctx, productID, "P2", "", nil)
	currencyID := insertDimRev(t, store, ctx, modelID, revID, "currency")
	addMember(t, store, ctx, currencyID, "EUR", "", nil)
	addMember(t, store, ctx, currencyID, "USD", "", nil)

	revenueID := insertMetric(t, store, ctx, modelID, revID, "revenue", "", true)
	profitID := insertMetric(t, store, ctx, modelID, revID, "profit", "", true)
	fxID := insertMetric(t, store, ctx, modelID, revID, "fx_rate", "", true)
	setMetricField(t, store, ctx, fxID, "agg_rule", "average")

	marginID := insertCalc(t, store, ctx, modelID, revID, "margin", "profit / revenue")
	setMetricField(t, store, ctx, marginID, "agg_rule", "formula")
	formulas := []struct{ name, f, agg string }{
		{"fxonly", "LOOKUP(fx_rate, currency, region.currency)", "sum"},
		{"lo", "revenue * region.weight", "average"},
		{"up", "revenue * REGION.weight", "average"},
		{"mworld", `LOOKUP(margin, region, "World")`, "sum"},
		{"mp1", `LOOKUP(margin, product, "P1")`, "formula"},
		{"x2", "margin * 2", "formula"},
		{"pw", "region.weight", "sum"},
		{"share_f", `revenue / LOOKUP(revenue, region, "World")`, "formula"},
	}
	ids := map[string]string{}
	labels := map[string]string{marginID: "margin"}
	calcIDs := []string{revenueID, profitID, marginID}
	for _, f := range formulas {
		id := insertCalc(t, store, ctx, modelID, revID, f.name, f.f)
		setMetricField(t, store, ctx, id, "agg_rule", f.agg)
		ids[f.name] = id
		labels[id] = f.name
		calcIDs = append(calcIDs, id)
	}
	insertGridSetup(t, store, ctx, modelID, []string{regionID, productID}, calcIDs)
	insertGridSetup(t, store, ctx, modelID, []string{currencyID}, []string{fxID})

	type cell struct{ region, product string }
	rev := map[cell]float64{
		{"DE", "P1"}: 60, {"DE", "P2"}: 40, {"FR", "P1"}: 150, {"FR", "P2"}: 50,
		{"US", "P1"}: 100, {"US", "P2"}: 200, {"UK", "P1"}: 10,
	}
	prof := map[cell]float64{
		{"DE", "P1"}: 6, {"DE", "P2"}: 4, {"FR", "P1"}: 45, {"FR", "P2"}: 5,
		{"US", "P1"}: 50, {"US", "P2"}: 20, {"UK", "P1"}: 1,
	}
	for k, v := range rev {
		insertFact(t, store, ctx, modelID, revID, revenueID, factJSON(regionID, k.region, productID, k.product), v)
	}
	for k, v := range prof {
		insertFact(t, store, ctx, modelID, revID, profitID, factJSON(regionID, k.region, productID, k.product), v)
	}
	insertFact(t, store, ctx, modelID, revID, fxID, factJSON(currencyID, "EUR"), 1.1)
	insertFact(t, store, ctx, modelID, revID, fxID, factJSON(currencyID, "USD"), 1.0)
	runRecalc(t, store, ctx, modelID, revID, []string{revenueID, profitID, fxID})

	c := rowChecker{t, store, ctx, modelID, revID, labels}
	rp := func(region, product string) map[string]string { return pins(regionID, region, productID, product) }

	// Defect: a LOOKUP-only formula whose member (a property) names a
	// missing member was skipped as "no data" and the partition stayed
	// clean. It is a failure naming the member.
	if status, msg := partitionState(t, store, ctx, ids["fxonly"]); status != "error" || !strings.Contains(msg, "#N/A") || !strings.Contains(msg, "GBP") {
		t.Errorf("fxonly: an unknown member through a property must fail with #N/A naming it: %s %q", status, msg)
	}
	if _, ok := calcRow(t, store, ctx, modelID, revID, ids["fxonly"], rp("UK", "P1")); ok {
		t.Error("fxonly @ UK: a #N/A cell must not persist a value")
	}
	c.want(ids["fxonly"], rp("DE", "P1"), 1.1)

	// Defect: REGION.weight was not seen as dimension-conditional, so the
	// average metric collapsed to one {} evaluation (blank property, 0).
	c.want(ids["up"], rp("DE", "P1"), 120)
	c.want(ids["up"], rp("US", "P2"), 1000)
	loTotal, loOK := calcRow(t, store, ctx, modelID, revID, ids["lo"], nil)
	upTotal, upOK := calcRow(t, store, ctx, modelID, revID, ids["up"], nil)
	// The mean of the eight leaves: 120, 80, 450, 150, 500, 1000 and UK's
	// two 0s (revenue times a blank weight).
	if !loOK || !upOK || loTotal != 287.5 || upTotal != 287.5 {
		t.Errorf("totals: up %v (row %v), lo %v (row %v), want 287.5 for both", upTotal, upOK, loTotal, loOK)
	}

	// Defect: LOOKUP of a formula-rule source at a parent returned the
	// mean of its children. It reads the persisted formula value.
	c.want(marginID, rp("World", "P1"), 102.0/320) // persisted: (6+45+50+1)/(60+150+100+10)
	c.want(ids["mworld"], rp("DE", "P1"), 102.0/320)
	c.want(ids["mworld"], rp("DE", "P2"), 29.0/290)
	c.want(ids["mworld"], rp("UK", "P2"), 29.0/290)
	c.want(marginID, pins(productID, "P1"), 102.0/320) // the one-dimension slice
	c.want(ids["mp1"], nil, 102.0/320)                 // LOOKUP to the slice row at the total
	c.want(ids["mp1"], rp("FR", "P2"), 0.3)            // FR/P1 leaf
	// ...and so does a plain reference above the leaves.
	c.want(ids["x2"], rp("EMEA", "P1"), 2*52.0/220)
	c.want(ids["x2"], nil, 2*131.0/610)

	// Defect: a blank result was persisted as 0. UK has no weight.
	c.want(ids["pw"], rp("DE", "P1"), 2)
	if v, ok := calcRow(t, store, ctx, modelID, revID, ids["pw"], rp("UK", "P1")); ok {
		t.Errorf("pw @ UK/P1: a blank result must persist no row, got %v", v)
	}
	c.want(ids["pw"], nil, 2*(2+3+5))

	c.want(ids["share_f"], nil, 1)
	for _, name := range []string{"lo", "up", "mworld", "mp1", "x2", "pw", "share_f"} {
		if status, msg := partitionState(t, store, ctx, ids[name]); status != "clean" {
			t.Errorf("%s: partition %s %q", name, status, msg)
		}
	}

	// Defect: with every revenue fact deleted, the per-combo rows went but
	// the '{}' total kept saying 100%.
	if _, err := store.Pool().Exec(ctx, `DELETE FROM runtime.fact_input WHERE metric_id=$1::uuid`, revenueID); err != nil {
		t.Fatal(err)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{revenueID})
	if v, ok := calcRow(t, store, ctx, modelID, revID, ids["share_f"], nil); ok {
		t.Errorf("share_f total after all revenue was deleted: got %v, want no row", v)
	}
	if _, ok := calcRow(t, store, ctx, modelID, revID, ids["share_f"], rp("DE", "P1")); ok {
		t.Error("share_f @ DE/P1 after all revenue was deleted: want no row")
	}
	if status, msg := partitionState(t, store, ctx, ids["share_f"]); status != "clean" {
		t.Errorf("share_f with no data: partition %s %q", status, msg)
	}
}

// TestDimensionalRegressionsTime:
//   - LOOKUP to an aggregate period of a nested hierarchy (FY > Q > month)
//     reduces the recorded months flat, agreeing with YEARVALUE (average
//     over unequally recorded quarters);
//   - LOOKUP of an agg_rule=formula source at an aggregate period reads the
//     persisted formula value, not the time summary of per-period ratios;
//   - YEARVALUE of a time_summary 'none' source is blank: no row;
//   - an unknown member through a property inside a time-series formula is
//     a failure, not "no data".
func TestDimensionalRegressionsTime(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000e2"
	revID := "00000000-0000-0000-0001-0000000000e2"

	monthID, months := monthHierarchy(t, store, ctx, modelID, revID, 1)
	declareProperty(t, store, ctx, monthID, "alt", "text")
	if _, err := store.Pool().Exec(ctx,
		`UPDATE model.dimension_member SET properties='{"alt":"Q7"}'::jsonb WHERE dimension_id=$1::uuid`, monthID); err != nil {
		t.Fatal(err)
	}
	baID := insertMetric(t, store, ctx, modelID, revID, "ba", "", true)
	setMetricField(t, store, ctx, baID, "time_summary", "average")
	bnID := insertMetric(t, store, ctx, modelID, revID, "bn", "", true)
	setMetricField(t, store, ctx, bnID, "time_summary", "none")
	numID := insertMetric(t, store, ctx, modelID, revID, "num", "", true)
	denID := insertMetric(t, store, ctx, modelID, revID, "den", "", true)
	ratioID := insertCalc(t, store, ctx, modelID, revID, "ratio", "num / den")
	setMetricField(t, store, ctx, ratioID, "agg_rule", "formula")

	formulas := map[string]string{
		"yv":  "YEARVALUE(ba)",
		"lk":  `LOOKUP(ba, month, "FY26")`,
		"lkq": `LOOKUP(ba, month, "Q1")`,
		"lkr": `LOOKUP(ratio, month, "Q1")`,
		"yvn": "YEARVALUE(bn)",
		"tna": "PREVIOUS(LOOKUP(ba, month, month.alt))",
	}
	ids := map[string]string{}
	labels := map[string]string{ratioID: "ratio"}
	all := []string{baID, bnID, numID, denID, ratioID}
	for name, f := range formulas {
		id := insertCalc(t, store, ctx, modelID, revID, name, f)
		ids[name] = id
		labels[id] = name
		all = append(all, id)
	}
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, all)
	// ba recorded in Jan, Mar, May, Jul, Sep, Nov: 1, 3, 5, 7, 9, 11 —
	// quarters with two, one, two and one recorded months.
	for i, v := range map[int]float64{0: 1, 2: 3, 4: 5, 6: 7, 8: 9, 10: 11} {
		insertFact(t, store, ctx, modelID, revID, baID, factJSON(monthID, months[i]), v)
	}
	for i := 0; i < 3; i++ {
		insertFact(t, store, ctx, modelID, revID, bnID, factJSON(monthID, months[i]), float64(i+1))
		insertFact(t, store, ctx, modelID, revID, numID, factJSON(monthID, months[i]), float64(i+1))
		insertFact(t, store, ctx, modelID, revID, denID, factJSON(monthID, months[i]), 10)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{baID, bnID, numID, denID})

	c := rowChecker{t, store, ctx, modelID, revID, labels}
	at := func(m string) map[string]string { return pins(monthID, m) }

	// Defect: LOOKUP to FY26 was a mean of quarter means (6.5).
	c.want(ids["yv"], at("2026-03"), 6)
	c.want(ids["lk"], at("2026-03"), 6)
	c.want(ids["lkq"], at("2026-08"), 2) // (1 + 3) / 2

	// Defect: LOOKUP(ratio, month, "Q1") was the sum of the monthly ratios
	// (0.6); the persisted Q1 ratio is 6/30.
	c.want(ratioID, at("Q1"), 0.2)
	c.want(ids["lkr"], at("2026-05"), 0.2)

	// Defect: YEARVALUE of a 'none' source (blank) was persisted as 0.
	for _, m := range []string{"2026-01", "2026-03", "2026-11"} {
		if v, ok := calcRow(t, store, ctx, modelID, revID, ids["yvn"], at(m)); ok {
			t.Errorf("yvn @ %s: a blank result must persist no row, got %v", m, v)
		}
	}

	// Defect (time-series path): the #N/A of an unknown member was "no
	// data" because nothing had been read before the member check.
	if status, msg := partitionState(t, store, ctx, ids["tna"]); status != "error" || !strings.Contains(msg, "#N/A") || !strings.Contains(msg, "Q7") {
		t.Errorf("tna: an unknown member must fail with #N/A naming it: %s %q", status, msg)
	}

	for _, name := range []string{"yv", "lk", "lkq", "lkr", "yvn"} {
		if status, msg := partitionState(t, store, ctx, ids[name]); status != "clean" {
			t.Errorf("%s: partition %s %q", name, status, msg)
		}
	}
}
