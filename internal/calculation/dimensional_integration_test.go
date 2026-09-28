package calculation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/timedim"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// Dimensional references through the real scheduler (contract C1-C5 of
// FORMULA_CALCULATION_INSTRUCTIONS.md): dim.property, PARENT, LOOKUP, the
// conditional aggregations and the time additions, with exact numbers at
// leaves, rollup combos, one-dimension slices and totals.

// addMember inserts a member with an optional parent and properties.
func addMember(t *testing.T, store *calculation.Store, ctx context.Context, dimID, code, parentID string, props map[string]string) string {
	t.Helper()
	var pid *string
	if parentID != "" {
		pid = &parentID
	}
	var propJSON []byte
	if props != nil {
		propJSON, _ = json.Marshal(props)
	}
	var id string
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, properties)
		VALUES ($1::uuid, $2, $2, $3::uuid, $4::jsonb) RETURNING id::text
	`, dimID, code, pid, propJSON).Scan(&id); err != nil {
		t.Fatalf("insert member %s: %v", code, err)
	}
	return id
}

func declareProperty(t *testing.T, store *calculation.Store, ctx context.Context, dimID, name, dataType string) {
	t.Helper()
	if _, err := store.Pool().Exec(ctx,
		`INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid, $2, $3)`, dimID, name, dataType); err != nil {
		t.Fatalf("declare property %s: %v", name, err)
	}
}

func setMetricField(t *testing.T, store *calculation.Store, ctx context.Context, metricID, field, value string) {
	t.Helper()
	if _, err := store.Pool().Exec(ctx, fmt.Sprintf(`UPDATE model.metric_def SET %s=$2 WHERE id=$1::uuid`, field), metricID, value); err != nil {
		t.Fatalf("set %s: %v", field, err)
	}
}

func partitionState(t *testing.T, store *calculation.Store, ctx context.Context, metricID string) (string, string) {
	t.Helper()
	var status, msg string
	if err := store.Pool().QueryRow(ctx,
		`SELECT status::text, COALESCE(error,'') FROM runtime.metric_partition_state WHERE metric_id=$1::uuid`, metricID).Scan(&status, &msg); err != nil {
		t.Fatalf("partition state: %v", err)
	}
	return status, msg
}

func factJSON(pairs ...string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func pins(pairs ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return m
}

type rowChecker struct {
	t      *testing.T
	store  *calculation.Store
	ctx    context.Context
	model  string
	rev    string
	labels map[string]string // metric ID -> name, for messages
}

func (c rowChecker) want(metricID string, dims map[string]string, want float64) {
	c.t.Helper()
	got, ok := calcRow(c.t, c.store, c.ctx, c.model, c.rev, metricID, dims)
	if !ok || math.Abs(got-want) > 1e-9 {
		c.t.Errorf("%s @ %v: got %v (row present=%v), want %v", c.labels[metricID], dims, got, ok, want)
	}
}

// regionFixture builds World > EMEA > {DE, FR}, World > AMER > {US} with
// typed properties.
func regionFixture(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revID string) string {
	t.Helper()
	regionID := insertDimRev(t, store, ctx, modelID, revID, "region")
	declareProperty(t, store, ctx, regionID, "weight", "number")
	declareProperty(t, store, ctx, regionID, "Currency", "text") // declared with a capital: matched case-insensitively
	declareProperty(t, store, ctx, regionID, "segment", "text")
	declareProperty(t, store, ctx, regionID, "opened", "date")
	world := addMember(t, store, ctx, regionID, "World", "", nil)
	emea := addMember(t, store, ctx, regionID, "EMEA", world, map[string]string{"Currency": "EUR"})
	amer := addMember(t, store, ctx, regionID, "AMER", world, map[string]string{"Currency": "USD"})
	addMember(t, store, ctx, regionID, "DE", emea, map[string]string{"weight": "2", "Currency": "EUR", "segment": "Ent", "opened": "2020-03-15"})
	addMember(t, store, ctx, regionID, "FR", emea, map[string]string{"weight": "3", "Currency": "EUR", "segment": "SMB", "opened": "2021-07-01"})
	addMember(t, store, ctx, regionID, "US", amer, map[string]string{"weight": "5", "Currency": "USD", "segment": "Ent", "opened": "2019-01-31"})
	return regionID
}

// TestDimensionalPropertyParentAndLookup: typed properties in arithmetic,
// PARENT, LOOKUP to a literal leaf and a literal parent (share of total)
// on leaves, rollups, slices and the total, a dynamic member through a
// property (FX), and unknown members failing unless IFNA handles them.
func TestDimensionalPropertyParentAndLookup(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000d1"
	revID := "00000000-0000-0000-0001-0000000000d1"

	regionID := regionFixture(t, store, ctx, modelID, revID)
	productID := insertDimRev(t, store, ctx, modelID, revID, "product")
	addMember(t, store, ctx, productID, "P1", "", nil)
	addMember(t, store, ctx, productID, "P2", "", nil)
	currencyID := insertDimRev(t, store, ctx, modelID, revID, "currency")
	addMember(t, store, ctx, currencyID, "EUR", "", nil)
	addMember(t, store, ctx, currencyID, "USD", "", nil)
	// XXX exists when "bad" and "ifna" are saved (a literal LOOKUP member is
	// checked at save, UNKNOWN_MEMBER) and is deleted afterwards: the
	// runtime #N/A below is what a member deleted after the save produces.
	addMember(t, store, ctx, currencyID, "XXX", "", nil)

	revenueID := insertMetric(t, store, ctx, modelID, revID, "revenue", "", true)
	fxID := insertMetric(t, store, ctx, modelID, revID, "fx_rate", "", true)
	setMetricField(t, store, ctx, fxID, "agg_rule", "average")

	formulas := []struct{ name, f, agg string }{
		{"weighted", "revenue * region.weight", "sum"},
		{"opened_year", "YEAR(region.OPENED)", "sum"},
		{"pflag", `IF(PARENT(region) = "EMEA", revenue, 0)`, "sum"},
		{"share", `revenue / LOOKUP(revenue, region, "World")`, "formula"},
		{"lk_de", `LOOKUP(revenue, region, "DE")`, "sum"},
		{"rev_fx", "revenue * IFNA(LOOKUP(fx_rate, currency, region.currency), 1)", "formula"},
		{"bad", `revenue * LOOKUP(fx_rate, currency, "XXX")`, "sum"},
		{"ifna", `IFNA(LOOKUP(fx_rate, currency, "XXX"), 7)`, "sum"},
	}
	ids := map[string]string{}
	labels := map[string]string{}
	calcIDs := []string{revenueID}
	for _, f := range formulas {
		id := insertCalc(t, store, ctx, modelID, revID, f.name, f.f)
		setMetricField(t, store, ctx, id, "agg_rule", f.agg)
		ids[f.name] = id
		labels[id] = f.name
		calcIDs = append(calcIDs, id)
	}
	insertGridSetup(t, store, ctx, modelID, []string{regionID, productID}, calcIDs)
	insertGridSetup(t, store, ctx, modelID, []string{currencyID}, []string{fxID})
	if _, err := store.Pool().Exec(ctx, `DELETE FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='XXX'`, currencyID); err != nil {
		t.Fatal(err)
	}

	rev := map[[2]string]float64{
		{"DE", "P1"}: 60, {"DE", "P2"}: 40,
		{"FR", "P1"}: 150, {"FR", "P2"}: 50,
		{"US", "P1"}: 100, {"US", "P2"}: 200,
	}
	for k, v := range rev {
		insertFact(t, store, ctx, modelID, revID, revenueID, factJSON(regionID, k[0], productID, k[1]), v)
	}
	insertFact(t, store, ctx, modelID, revID, fxID, factJSON(currencyID, "EUR"), 1.1)
	insertFact(t, store, ctx, modelID, revID, fxID, factJSON(currencyID, "USD"), 1.0)
	runRecalc(t, store, ctx, modelID, revID, []string{revenueID, fxID})

	c := rowChecker{t, store, ctx, modelID, revID, labels}
	rp := func(region, product string) map[string]string {
		return pins(regionID, region, productID, product)
	}

	// A number-typed property is a number in arithmetic.
	c.want(ids["weighted"], rp("FR", "P2"), 150)
	c.want(ids["weighted"], rp("US", "P1"), 500)
	c.want(ids["weighted"], nil, 2300)
	// A date-typed property is a date serial.
	c.want(ids["opened_year"], rp("DE", "P1"), 2020)
	c.want(ids["opened_year"], rp("US", "P2"), 2019)
	// PARENT.
	c.want(ids["pflag"], rp("DE", "P1"), 60)
	c.want(ids["pflag"], rp("FR", "P2"), 50)
	c.want(ids["pflag"], rp("US", "P2"), 0)

	// Share of total: the literal parent World combines its leaves, with
	// the product pin kept.
	c.want(ids["share"], rp("DE", "P1"), 60.0/310)
	c.want(ids["share"], rp("US", "P2"), 200.0/290)
	c.want(ids["share"], rp("EMEA", "P1"), 210.0/310) // rollup combo
	c.want(ids["share"], rp("World", "P2"), 1)
	c.want(ids["share"], pins(regionID, "EMEA"), 300.0/600) // one-dimension slices
	c.want(ids["share"], pins(regionID, "US"), 300.0/600)
	c.want(ids["share"], pins(productID, "P1"), 1)
	c.want(ids["share"], nil, 1)

	// LOOKUP to a literal leaf keeps the other pins.
	c.want(ids["lk_de"], rp("US", "P2"), 40)
	c.want(ids["lk_de"], rp("FR", "P1"), 60)

	// FX through a property: fx_rate is dimensioned by currency only, so a
	// region parent reads the one rate — never the rate times its children.
	c.want(ids["rev_fx"], rp("DE", "P1"), 66)
	c.want(ids["rev_fx"], rp("US", "P2"), 200)
	c.want(ids["rev_fx"], rp("EMEA", "P1"), 210*1.1)
	c.want(ids["rev_fx"], rp("AMER", "P2"), 200)
	c.want(ids["rev_fx"], rp("World", "P1"), 310) // World declares no currency: IFNA's 1
	c.want(ids["rev_fx"], pins(regionID, "EMEA"), 300*1.1)
	c.want(ids["rev_fx"], pins(productID, "P1"), 310)
	c.want(ids["rev_fx"], nil, 600)

	// An unknown member is #N/A: a failure with data present...
	if status, msg := partitionState(t, store, ctx, ids["bad"]); status != "error" || !strings.Contains(msg, "#N/A") || !strings.Contains(msg, "XXX") {
		t.Errorf("unknown LOOKUP member must fail with #N/A naming it: %s %q", status, msg)
	}
	if _, ok := calcRow(t, store, ctx, modelID, revID, ids["bad"], rp("DE", "P1")); ok {
		t.Error("a #N/A cell must not persist a value")
	}
	// ...and IFNA handles it.
	c.want(ids["ifna"], rp("DE", "P1"), 7)
	c.want(ids["ifna"], rp("US", "P2"), 7)
	for _, name := range []string{"weighted", "share", "lk_de", "rev_fx", "ifna"} {
		if status, msg := partitionState(t, store, ctx, ids[name]); status != "clean" {
			t.Errorf("%s: partition %s %q", name, status, msg)
		}
	}
}

// TestConditionalAggregation: SUMIFS by a typed property, AVERAGEIFS
// skipping absent intersections, MINIFS/MAXIFS, COUNTIFS across two
// dimensions, the same-segment sum, and slice/rollup rows of LOOKUP/SUMIFS
// metrics equal to a direct recomputation from the data.
func TestConditionalAggregation(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000d2"
	revID := "00000000-0000-0000-0001-0000000000d2"

	regionID := regionFixture(t, store, ctx, modelID, revID)
	channelID := insertDimRev(t, store, ctx, modelID, revID, "channel")
	addMember(t, store, ctx, channelID, "Online", "", nil)
	addMember(t, store, ctx, channelID, "Retail", "", nil)
	salesID := insertMetric(t, store, ctx, modelID, revID, "sales", "", true)

	formulas := []struct{ name, f, agg string }{
		{"ent", `SUMIFS(sales, region.segment, "Ent")`, "formula"},
		{"heavy", `SUMIFS(sales, region.weight, ">2.5")`, "sum"},
		{"avg_retail", `AVERAGEIFS(sales, region, "*", channel, "Retail")`, "sum"},
		{"mn", `MINIFS(sales, region, "<>FR", channel, "Online")`, "sum"},
		{"mx", `MAXIFS(sales, region, "<>FR", channel, "Online")`, "sum"},
		{"mx_none", `MAXIFS(sales, region, "FR", channel, "Retail")`, "sum"},
		{"cnt", `COUNTIFS(region.segment, "Ent", channel, "*")`, "sum"},
		{"same_seg", "SUMIFS(sales, region.segment, region.segment)", "sum"},
		{"sumif", `SUMIF(region, "?E", sales)`, "sum"},
		{"lk_us", `LOOKUP(sales, region, "US")`, "formula"},
	}
	ids := map[string]string{}
	labels := map[string]string{}
	all := []string{salesID}
	for _, f := range formulas {
		id := insertCalc(t, store, ctx, modelID, revID, f.name, f.f)
		setMetricField(t, store, ctx, id, "agg_rule", f.agg)
		ids[f.name] = id
		labels[id] = f.name
		all = append(all, id)
	}
	insertGridSetup(t, store, ctx, modelID, []string{regionID, channelID}, all)
	for _, f := range []struct {
		r, ch string
		v     float64
	}{{"DE", "Online", 10}, {"DE", "Retail", 20}, {"FR", "Online", 35}, {"US", "Online", 40}, {"US", "Retail", 50}} {
		insertFact(t, store, ctx, modelID, revID, salesID, factJSON(regionID, f.r, channelID, f.ch), f.v)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{salesID})

	c := rowChecker{t, store, ctx, modelID, revID, labels}
	rc := func(region, channel string) map[string]string {
		return pins(regionID, region, channelID, channel)
	}
	// SUMIFS by a text property; the channel pin is kept.
	c.want(ids["ent"], rc("FR", "Online"), 10+40)
	c.want(ids["ent"], rc("DE", "Retail"), 20+50)
	c.want(ids["ent"], rc("EMEA", "Online"), 10+40)   // rollup combo
	c.want(ids["ent"], pins(regionID, "EMEA"), 30+90) // slice: every channel
	c.want(ids["ent"], pins(channelID, "Retail"), 70)
	c.want(ids["ent"], nil, 120)
	// SUMIFS by a number property with a comparison.
	c.want(ids["heavy"], rc("DE", "Online"), 35+40)
	c.want(ids["heavy"], rc("DE", "Retail"), 50)
	// AVERAGEIFS skips FR/Retail, which has no value: (20+50)/2, not /3.
	c.want(ids["avg_retail"], rc("FR", "Online"), 35)
	c.want(ids["mn"], rc("US", "Retail"), 10)
	c.want(ids["mx"], rc("US", "Retail"), 40)
	c.want(ids["mx_none"], rc("DE", "Online"), 0) // nothing recorded: 0, as in Excel
	// COUNTIFS counts member tuples across two dimensions: {DE, US} x {Online, Retail}.
	c.want(ids["cnt"], rc("FR", "Retail"), 4)
	// Same-segment sum: each region's own segment, channel pin kept.
	c.want(ids["same_seg"], rc("DE", "Online"), 50)
	c.want(ids["same_seg"], rc("FR", "Online"), 35)
	c.want(ids["same_seg"], rc("FR", "Retail"), 0)
	c.want(ids["same_seg"], rc("US", "Retail"), 70)
	// A sum metric's slices combine its leaves.
	c.want(ids["same_seg"], pins(regionID, "EMEA"), 50+70+35+0)
	c.want(ids["same_seg"], pins(channelID, "Online"), 50+35+50)
	// SUMIF in Excel argument order, with a wildcard: DE only ("?E").
	c.want(ids["sumif"], rc("US", "Online"), 10)
	// LOOKUP under agg_rule formula: slices and rollups re-evaluate it.
	c.want(ids["lk_us"], pins(channelID, "Retail"), 50)
	c.want(ids["lk_us"], pins(regionID, "EMEA"), 90)
	c.want(ids["lk_us"], rc("World", "Online"), 40)
	c.want(ids["lk_us"], nil, 90)
	for name, id := range ids {
		if status, msg := partitionState(t, store, ctx, id); status != "clean" {
			t.Errorf("%s: partition %s %q", name, status, msg)
		}
	}
}

// monthHierarchy builds FY26 > Q1..Q4 > the twelve months of 2026 on a
// monthly time dimension with the given fiscal start month.
func monthHierarchy(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revID string, fiscalStart int) (string, []string) {
	t.Helper()
	monthID := insertTimeDim(t, store, ctx, modelID, revID, "month")
	if _, err := store.Pool().Exec(ctx, `UPDATE model.dimension_def SET fiscal_year_start_month=$2 WHERE id=$1::uuid`, monthID, fiscalStart); err != nil {
		t.Fatal(err)
	}
	tx, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	add := func(code, parent, start string) string {
		var pid *string
		if parent != "" {
			pid = &parent
		}
		var id string
		if start == "" {
			err = tx.QueryRow(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid, $2, $2, $3::uuid) RETURNING id::text`,
				monthID, code, pid).Scan(&id)
		} else {
			err = tx.QueryRow(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, period_start, period_end, time_index)
				VALUES ($1::uuid, $2, $2, $3::uuid, $4::date, ($4::date + interval '1 month' - interval '1 day')::date, 0) RETURNING id::text`,
				monthID, code, pid, start).Scan(&id)
		}
		if err != nil {
			t.Fatalf("add %s: %v", code, err)
		}
		return id
	}
	fy := add("FY26", "", "")
	var months []string
	for q := 1; q <= 4; q++ {
		qid := add(fmt.Sprintf("Q%d", q), fy, "")
		for m := 3*q - 2; m <= 3*q; m++ {
			code := fmt.Sprintf("2026-%02d", m)
			add(code, qid, code+"-01")
			months = append(months, code)
		}
	}
	if err := timedim.ValidateAndReindex(ctx, tx, monthID); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return monthID, months
}

// TestTimeAdditionsThroughScheduler: dynamic LAG offsets, the *VALUE family
// by time_summary with a fiscal start month of 4, HALFYEARTODATE, TIMESUM
// over aggregate periods, START/END, LOOKUP of the time dimension to an
// aggregate period, and LOOKUP/PARENT evaluated inside a time-shifted
// child.
func TestTimeAdditionsThroughScheduler(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000d3"
	revID := "00000000-0000-0000-0001-0000000000d3"

	monthID, months := monthHierarchy(t, store, ctx, modelID, revID, 4)
	salesID := insertMetric(t, store, ctx, modelID, revID, "sales", "", true)
	balID := insertMetric(t, store, ctx, modelID, revID, "bal", "", true)
	setMetricField(t, store, ctx, balID, "time_summary", "last")
	lagmID := insertMetric(t, store, ctx, modelID, revID, "lagm", "", true)

	formulas := map[string]string{
		"dyn":  "LAG(sales, lagm, 0)",
		"yv":   "YEARVALUE(sales)",
		"yvl":  "YEARVALUE(bal)",
		"hytd": "HALFYEARTODATE(sales)",
		"tsr":  `TIMESUM(sales, "Q2", "Q3")`,
		"tsa":  "TIMESUM(sales)",
		"days": "DAYS(END(), START()) + 1",
		"fyl":  `LOOKUP(bal, month, "FY26")`,
		"q2":   `LOOKUP(sales, month, "Q2")`,
		"mix":  "PREVIOUS(LOOKUP(bal, month, PARENT(month)))",
		"mix2": `LAG(sales, 1, 0) + LOOKUP(sales, month, "Q1")`,
	}
	ids := map[string]string{}
	labels := map[string]string{}
	all := []string{salesID, balID, lagmID}
	for name, f := range formulas {
		id := insertCalc(t, store, ctx, modelID, revID, name, f)
		ids[name] = id
		labels[id] = name
		all = append(all, id)
	}
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, all)
	for i, m := range months {
		insertFact(t, store, ctx, modelID, revID, salesID, factJSON(monthID, m), float64(10*(i+1)))
		insertFact(t, store, ctx, modelID, revID, balID, factJSON(monthID, m), float64(100+i+1))
		lag := 1.0
		if m == "2026-06" {
			lag = 2
		}
		insertFact(t, store, ctx, modelID, revID, lagmID, factJSON(monthID, m), lag)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{salesID, balID, lagmID})

	c := rowChecker{t, store, ctx, modelID, revID, labels}
	at := func(m string) map[string]string { return pins(monthID, m) }
	// Dynamic offset from an input metric: 1 everywhere, 2 in June.
	c.want(ids["dyn"], at("2026-01"), 0) // before the first period: the substitute
	c.want(ids["dyn"], at("2026-02"), 10)
	c.want(ids["dyn"], at("2026-06"), 40)
	c.want(ids["dyn"], at("2026-07"), 60)
	// Fiscal year from April: Jan-Mar 2026 close FY Apr25-Mar26.
	c.want(ids["yv"], at("2026-01"), 10+20+30)
	c.want(ids["yv"], at("2026-05"), 40+50+60+70+80+90+100+110+120)
	c.want(ids["yvl"], at("2026-02"), 103) // last of Jan..Mar
	c.want(ids["yvl"], at("2026-05"), 112) // last of Apr..Dec
	// Fiscal halves Apr-Sep and Oct-Mar.
	c.want(ids["hytd"], at("2026-01"), 10)
	c.want(ids["hytd"], at("2026-03"), 60)
	c.want(ids["hytd"], at("2026-04"), 40)
	c.want(ids["hytd"], at("2026-09"), 40+50+60+70+80+90)
	c.want(ids["hytd"], at("2026-12"), 100+110+120)
	// TIMESUM: aggregate start = its first leaf, aggregate end = its last.
	c.want(ids["tsr"], at("2026-01"), 40+50+60+70+80+90)
	c.want(ids["tsa"], at("2026-08"), 780)
	// START/END: inclusive days in the period.
	c.want(ids["days"], at("2026-01"), 31)
	c.want(ids["days"], at("2026-02"), 28)
	c.want(ids["days"], at("2026-04"), 30)
	// LOOKUP to an aggregate period reduces by the source's time_summary.
	c.want(ids["fyl"], at("2026-03"), 112) // bal: last
	c.want(ids["q2"], at("2026-11"), 40+50+60)
	// PARENT and LOOKUP inside PREVIOUS see the SHIFTED period.
	c.want(ids["mix"], at("2026-01"), 0)
	c.want(ids["mix"], at("2026-04"), 103) // Mar -> Q1 -> bal last of Q1
	c.want(ids["mix"], at("2026-05"), 106) // Apr -> Q2 -> bal Jun
	c.want(ids["mix2"], at("2026-02"), 10+60)
	for name, id := range ids {
		if status, msg := partitionState(t, store, ctx, id); status != "clean" {
			t.Errorf("%s: partition %s %q", name, status, msg)
		}
	}
}

// TestRecurrenceRefusesDimensionalFunctions: a recurrence member calling
// LOOKUP (a graph written around the validator) fails at run time with
// TEMPORAL_CYCLE_NOT_CAUSAL naming the function, and persists nothing.
func TestRecurrenceRefusesDimensionalFunctions(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000d4"
	revID := "00000000-0000-0000-0001-0000000000d4"

	monthID := insertTimeDim(t, store, ctx, modelID, revID, "month")
	months := insertMonths(t, store, ctx, monthID, 3)
	flowID := insertMetric(t, store, ctx, modelID, revID, "flow", "", true)
	closingID := insertMetric(t, store, ctx, modelID, revID, "closing", "opening + flow", false)
	openingID := insertCalc(t, store, ctx, modelID, revID, "opening", "LAG(closing, 1, 100)")
	insertDep(t, store, ctx, closingID, flowID)
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)`, closingID, openingID); err != nil {
		t.Fatal(err)
	}
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, []string{flowID, closingID, openingID})
	for i, v := range []float64{10, -20, 5} {
		insertFact(t, store, ctx, modelID, revID, flowID, factJSON(monthID, months[i]), v)
	}
	// Written around the validator: the formula text gains a LOOKUP.
	if _, err := store.Pool().Exec(ctx, `UPDATE model.metric_def SET formula=$2 WHERE id=$1::uuid`,
		closingID, `opening + flow + LOOKUP(flow, month, "2026-01") * 0`); err != nil {
		t.Fatal(err)
	}
	sched := calculation.NewScheduler(logger.New("calc-test"), store, nil)
	recalcErr := sched.RecalcAffected(ctx, modelID, revID, []string{flowID})
	refused := recalcErr != nil && strings.Contains(recalcErr.Error(), "TEMPORAL_CYCLE_NOT_CAUSAL")
	if !refused {
		status, msg := partitionState(t, store, ctx, closingID)
		if status != "error" || !strings.Contains(msg, "TEMPORAL_CYCLE_NOT_CAUSAL") || !strings.Contains(msg, "LOOKUP") {
			t.Fatalf("a recurrence calling LOOKUP must fail with TEMPORAL_CYCLE_NOT_CAUSAL naming LOOKUP: recalc=%v, partition %s %q", recalcErr, status, msg)
		}
	}
	for _, id := range []string{closingID, openingID} {
		if _, ok := calcRow(t, store, ctx, modelID, revID, id, pins(monthID, months[1])); ok {
			t.Errorf("metric %s: a refused recurrence must persist no rows", id)
		}
	}
}

// TestConditionalAggregationPerformance: SUMIFS over 300 leaf members x 12
// periods — 3,600 cells each iterating 300 members — stays fast because
// the memo answers every cell of a (segment, period) once.
func TestConditionalAggregationPerformance(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000d5"
	revID := "00000000-0000-0000-0001-0000000000d5"

	regionID := insertDimRev(t, store, ctx, modelID, revID, "region")
	declareProperty(t, store, ctx, regionID, "segment", "text")
	world := addMember(t, store, ctx, regionID, "World", "", nil)
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, properties, sort_order)
		SELECT $1::uuid, 'R' || g, 'R' || g, $2::uuid, jsonb_build_object('segment', 'S' || (g % 3)), g
		FROM generate_series(1, 300) g`, regionID, world); err != nil {
		t.Fatal(err)
	}
	monthID := insertTimeDim(t, store, ctx, modelID, revID, "month")
	insertMonths(t, store, ctx, monthID, 12)
	salesID := insertMetric(t, store, ctx, modelID, revID, "sales", "", true)
	segID := insertCalc(t, store, ctx, modelID, revID, "seg", "SUMIFS(sales, region.segment, region.segment)")
	insertGridSetup(t, store, ctx, modelID, []string{regionID, monthID}, []string{salesID, segID})
	// sales = the region's number, every month.
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		SELECT $1::uuid, $2::uuid, $3::uuid, jsonb_build_object($6::text, r.code, $7::text, m.code), substr(r.code, 2)::int
		FROM model.dimension_member r, model.dimension_member m
		WHERE r.dimension_id = $4::uuid AND r.code <> 'World' AND m.dimension_id = $5::uuid`,
		modelID, revID, salesID, regionID, monthID, regionID, monthID); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	runRecalc(t, store, ctx, modelID, revID, []string{salesID})
	elapsed := time.Since(start)
	t.Logf("SUMIFS over 300 members x 12 periods: recalculation took %v", elapsed)
	if elapsed > 10*time.Second {
		t.Errorf("recalculation took %v, want well under 10s", elapsed)
	}
	// Segment S1 = members 1, 4, ..., 298: 100 members summing to 14,950.
	var s1 float64
	for g := 1; g <= 300; g++ {
		if g%3 == 1 {
			s1 += float64(g)
		}
	}
	got, ok := calcRow(t, store, ctx, modelID, revID, segID, pins(regionID, "R4", monthID, "2026-07"))
	if !ok || math.Abs(got-s1) > 1e-9 {
		t.Errorf("seg @ R4/July: got %v ok=%v want %v", got, ok, s1)
	}
	if status, msg := partitionState(t, store, ctx, segID); status != "clean" {
		t.Errorf("partition %s %q", status, msg)
	}
}
