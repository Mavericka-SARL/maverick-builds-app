package calculation_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/timedim"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// Time-series integration tests: the exact acceptance fixture of spec §11
// run through the real scheduler against real tables — leaf periods, the
// time-summary aggregate, non-time slices that must not bleed into each
// other, and the opening/closing recurrence persisted atomically.

func insertTimeDim(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revID, name string) string {
	t.Helper()
	var id string
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid, $2::uuid, $3, 'time', 'month', 1) RETURNING id::text
	`, modelID, revID, name).Scan(&id); err != nil {
		t.Fatalf("insert time dim: %v", err)
	}
	return id
}

// insertMonths creates n monthly members from Jan 2026 through the shared
// timedim validation + reindex path, exactly as the developer API does.
func insertMonths(t *testing.T, store *calculation.Store, ctx context.Context, dimID string, n int) []string {
	t.Helper()
	tx, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var codes []string
	for i := 0; i < n; i++ {
		code := fmt.Sprintf("2026-%02d", i+1)
		start := fmt.Sprintf("2026-%02d-01", i+1)
		if _, err := tx.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index)
			VALUES ($1::uuid, $2, $2, $3::date, ($3::date + interval '1 month' - interval '1 day')::date, 0)
		`, dimID, code, start); err != nil {
			t.Fatalf("insert month %s: %v", code, err)
		}
		codes = append(codes, code)
	}
	if err := timedim.ValidateAndReindex(ctx, tx, dimID); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return codes
}

// insertCalc creates a calculated metric and its dependency edges through
// the shared validator — the same path the developer API takes — so the
// offsets on calc_dependency are the real ones.
func insertCalc(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revID, name, f string) string {
	t.Helper()
	res, err := metricformula.Validate(ctx, store.Pool(), metricformula.Request{ModelID: modelID, RevisionID: revID, Name: name, Formula: f})
	if err != nil {
		t.Fatalf("validate %s: %v", name, err)
	}
	id := insertMetric(t, store, ctx, modelID, revID, name, f, false)
	if err := metricformula.WriteDependencies(ctx, store.Pool(), id, res.Edges); err != nil {
		t.Fatalf("write deps %s: %v", name, err)
	}
	return id
}

func calcRow(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revID, metricID string, dims map[string]string) (float64, bool) {
	t.Helper()
	dimJSON := "{}"
	if len(dims) > 0 {
		parts := make([]string, 0, len(dims))
		for k, v := range dims {
			parts = append(parts, fmt.Sprintf("%q:%q", k, v))
		}
		dimJSON = "{" + strings.Join(parts, ",") + "}"
	}
	var v *float64
	err := store.Pool().QueryRow(ctx, `
		SELECT value FROM runtime.calc_result
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND dim_members=$4::jsonb
		ORDER BY calc_at DESC LIMIT 1`, modelID, revID, metricID, dimJSON).Scan(&v)
	if err != nil || v == nil {
		return 0, false
	}
	return *v, true
}

func TestTimeSeriesSpecFixture(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000a1"
	revID := "00000000-0000-0000-0001-0000000000a1"

	monthID := insertTimeDim(t, store, ctx, modelID, revID, "month")
	months := insertMonths(t, store, ctx, monthID, 4)
	salesID := insertMetric(t, store, ctx, modelID, revID, "sales", "", true)
	formulas := map[string]string{
		"prev":   "PREVIOUS(sales)",
		"next":   "NEXT(sales)",
		"lag2":   "LAG(sales, 2, 0)",
		"lead1":  "LEAD(sales, 1, 0)",
		"offm1":  "OFFSET(sales, -1, 0)",
		"mov":    "MOVINGSUM(sales, -2, 0)",
		"movavg": "MOVINGSUM(sales, -2, 0, AVERAGE)",
		"cum":    "CUMULATE(sales)",
		"decum":  "DECUMULATE(sales)",
		"qtd":    "QUARTERTODATE(sales)",
	}
	want := map[string][]float64{
		"prev":   {0, 100, 120, 80},
		"next":   {120, 80, 150, 0},
		"lag2":   {0, 0, 100, 120},
		"lead1":  {120, 80, 150, 0},
		"offm1":  {0, 100, 120, 80},
		"mov":    {100, 220, 300, 350},
		"movavg": {100, 110, 100, 116.6667},
		"cum":    {100, 220, 300, 450},
		"decum":  {100, 20, -40, 70},
		"qtd":    {100, 220, 300, 150},
	}
	ids := map[string]string{}
	all := []string{salesID}
	for name, f := range formulas {
		ids[name] = insertCalc(t, store, ctx, modelID, revID, name, f)
		all = append(all, ids[name])
	}
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, all)
	for i, v := range []float64{100, 120, 80, 150} {
		insertFact(t, store, ctx, modelID, revID, salesID, fmt.Sprintf(`{"%s":"%s"}`, monthID, months[i]), v)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{salesID})

	for name, exp := range want {
		for i, code := range months {
			got, ok := calcRow(t, store, ctx, modelID, revID, ids[name], map[string]string{monthID: code})
			if !ok {
				t.Errorf("%s @ %s: no persisted row", name, code)
				continue
			}
			if math.Abs(got-exp[i]) > 1e-3 {
				t.Errorf("%s @ %s: got %v, want %v", name, code, got, exp[i])
			}
		}
	}
	// Default time_summary 'sum' totals the leaf periods.
	if got, ok := calcRow(t, store, ctx, modelID, revID, ids["cum"], nil); !ok || math.Abs(got-1070) > 1e-6 {
		t.Errorf("CUMULATE total (sum over periods): got %v ok=%v, want 1070", got, ok)
	}

	// A closing-balance style summary: switch cum to 'last' and recalculate —
	// leaf values must not change, the total becomes the last period's.
	if _, err := store.Pool().Exec(ctx, `UPDATE model.metric_def SET time_summary='last' WHERE id=$1::uuid`, ids["cum"]); err != nil {
		t.Fatal(err)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{salesID})
	if got, _ := calcRow(t, store, ctx, modelID, revID, ids["cum"], nil); math.Abs(got-450) > 1e-6 {
		t.Errorf("CUMULATE total with time_summary=last: got %v, want 450", got)
	}
	if got, _ := calcRow(t, store, ctx, modelID, revID, ids["cum"], map[string]string{monthID: months[1]}); math.Abs(got-220) > 1e-6 {
		t.Errorf("leaf value must be unchanged by the summary method: got %v", got)
	}

	// A changed earlier input updates every later cumulative period.
	insertFact(t, store, ctx, modelID, revID, salesID, fmt.Sprintf(`{"%s":"%s"}`, monthID, months[0]), 200)
	runRecalc(t, store, ctx, modelID, revID, []string{salesID})
	for i, exp := range []float64{200, 320, 400, 550} {
		if got, _ := calcRow(t, store, ctx, modelID, revID, ids["cum"], map[string]string{monthID: months[i]}); math.Abs(got-exp) > 1e-6 {
			t.Errorf("after Jan change, CUMULATE @ %s: got %v, want %v", months[i], got, exp)
		}
	}
}

func TestTimeSeriesSlicesDoNotBleed(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000a2"
	revID := "00000000-0000-0000-0001-0000000000a2"

	monthID := insertTimeDim(t, store, ctx, modelID, revID, "month")
	months := insertMonths(t, store, ctx, monthID, 4)
	productID := insertDimRev(t, store, ctx, modelID, revID, "product")
	insertMember(t, store, ctx, productID, "A", "A")
	insertMember(t, store, ctx, productID, "B", "B")
	salesID := insertMetric(t, store, ctx, modelID, revID, "sales", "", true)
	movID := insertCalc(t, store, ctx, modelID, revID, "mov", "MOVINGSUM(sales, -2, 0)")
	cumID := insertCalc(t, store, ctx, modelID, revID, "cum", "CUMULATE(sales)")
	insertGridSetup(t, store, ctx, modelID, []string{monthID, productID}, []string{salesID, movID, cumID})
	a := []float64{100, 120, 80, 150}
	b := []float64{10, 20, 30, 40}
	for i := range months {
		insertFact(t, store, ctx, modelID, revID, salesID, fmt.Sprintf(`{"%s":"%s","%s":"A"}`, monthID, months[i], productID), a[i])
		insertFact(t, store, ctx, modelID, revID, salesID, fmt.Sprintf(`{"%s":"%s","%s":"B"}`, monthID, months[i], productID), b[i])
	}
	runRecalc(t, store, ctx, modelID, revID, []string{salesID})

	wantA := []float64{100, 220, 300, 350}
	wantB := []float64{10, 30, 60, 90}
	for i, code := range months {
		if got, _ := calcRow(t, store, ctx, modelID, revID, movID, map[string]string{monthID: code, productID: "A"}); math.Abs(got-wantA[i]) > 1e-6 {
			t.Errorf("A @ %s: got %v want %v", code, got, wantA[i])
		}
		if got, _ := calcRow(t, store, ctx, modelID, revID, movID, map[string]string{monthID: code, productID: "B"}); math.Abs(got-wantB[i]) > 1e-6 {
			t.Errorf("B @ %s: got %v want %v", code, got, wantB[i])
		}
	}
	// Period slice (product aggregated by agg_rule sum): A+B per period.
	if got, _ := calcRow(t, store, ctx, modelID, revID, movID, map[string]string{monthID: months[3]}); math.Abs(got-440) > 1e-6 {
		t.Errorf("period slice: got %v want 440", got)
	}
	// Product slice (time reduced by time_summary sum): sum over periods of A.
	if got, _ := calcRow(t, store, ctx, modelID, revID, movID, map[string]string{productID: "A"}); math.Abs(got-970) > 1e-6 {
		t.Errorf("product slice: got %v want 970", got)
	}
	// Grand total: non-time first, then time. With sum/sum it is the full sum.
	if got, _ := calcRow(t, store, ctx, modelID, revID, cumID, nil); math.Abs(got-(1070+200)) > 1e-6 {
		t.Errorf("grand total: got %v want 1270", got)
	}
}

func TestTimeSeriesRecurrence(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000a3"
	revID := "00000000-0000-0000-0001-0000000000a3"

	monthID := insertTimeDim(t, store, ctx, modelID, revID, "month")
	months := insertMonths(t, store, ctx, monthID, 3)
	flowID := insertMetric(t, store, ctx, modelID, revID, "net_cash_flow", "", true)
	// closing must exist before opening can reference it; opening's own
	// edge is written first with closing as a plain (not yet cyclic) edge.
	closingID := insertMetric(t, store, ctx, modelID, revID, "closing_cash", "opening_cash + net_cash_flow", false)
	openingID := insertCalc(t, store, ctx, modelID, revID, "opening_cash", "LAG(closing_cash, 1, 100)")
	res, err := metricformula.Validate(ctx, store.Pool(), metricformula.Request{
		ModelID: modelID, RevisionID: revID, MetricID: closingID, Name: "closing_cash", Formula: "opening_cash + net_cash_flow"})
	if err != nil {
		t.Fatalf("the opening/closing pair must validate as a causal recurrence: %v", err)
	}
	if err := metricformula.WriteDependencies(ctx, store.Pool(), closingID, res.Edges); err != nil {
		t.Fatal(err)
	}
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, []string{flowID, closingID, openingID})
	for i, v := range []float64{10, -20, 5} {
		insertFact(t, store, ctx, modelID, revID, flowID, fmt.Sprintf(`{"%s":"%s"}`, monthID, months[i]), v)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{flowID})

	wantOpen := []float64{100, 110, 90}
	wantClose := []float64{110, 90, 95}
	for i, code := range months {
		if got, ok := calcRow(t, store, ctx, modelID, revID, openingID, map[string]string{monthID: code}); !ok || math.Abs(got-wantOpen[i]) > 1e-6 {
			t.Errorf("opening @ %s: got %v ok=%v want %v", code, got, ok, wantOpen[i])
		}
		if got, ok := calcRow(t, store, ctx, modelID, revID, closingID, map[string]string{monthID: code}); !ok || math.Abs(got-wantClose[i]) > 1e-6 {
			t.Errorf("closing @ %s: got %v ok=%v want %v", code, got, ok, wantClose[i])
		}
	}

	// A non-causal cycle is refused at save time, not deferred to the scheduler.
	_, err = metricformula.Validate(ctx, store.Pool(), metricformula.Request{
		ModelID: modelID, RevisionID: revID, MetricID: openingID, Name: "opening_cash", Formula: "closing_cash - 1"})
	var ve *metricformula.ValidationError
	if err == nil || !strings.Contains(err.Error(), "TEMPORAL_CYCLE_NOT_CAUSAL") {
		t.Errorf("same-period cycle must be rejected with TEMPORAL_CYCLE_NOT_CAUSAL: %v", err)
	}
	_ = ve
	_, err = metricformula.Validate(ctx, store.Pool(), metricformula.Request{
		ModelID: modelID, RevisionID: revID, MetricID: openingID, Name: "opening_cash", Formula: "CUMULATE(closing_cash)"})
	if err == nil || !strings.Contains(err.Error(), "TEMPORAL_CYCLE_NOT_CAUSAL") {
		t.Errorf("unbounded edge in a cycle must be rejected: %v", err)
	}
	_, err = metricformula.Validate(ctx, store.Pool(), metricformula.Request{
		ModelID: modelID, RevisionID: revID, MetricID: openingID, Name: "opening_cash", Formula: "LEAD(closing_cash, 1, 0) + LAG(closing_cash, 1, 0)"})
	if err == nil || !strings.Contains(err.Error(), "TEMPORAL_CYCLE_NOT_CAUSAL") {
		t.Errorf("mixed-direction cycle must be rejected: %v", err)
	}
	// A strictly-past self reference is legal and calculates.
	if _, err := metricformula.Validate(ctx, store.Pool(), metricformula.Request{
		ModelID: modelID, RevisionID: revID, Name: "running", Formula: "PREVIOUS(running) + net_cash_flow"}); err != nil {
		t.Errorf("past self-reference must be legal: %v", err)
	}
}

// TestTimeFunctionWithoutTimeDimensionFails: a dimension called "month"
// that is NOT marked time never acts as one, and the metric errors with
// TIME_CONTEXT_REQUIRED instead of producing zeros.
func TestTimeFunctionWithoutTimeDimensionFails(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000a4"
	revID := "00000000-0000-0000-0001-0000000000a4"

	monthID := insertDimRev(t, store, ctx, modelID, revID, "month")
	insertMember(t, store, ctx, monthID, "2026-01", "Jan")
	insertMember(t, store, ctx, monthID, "2026-02", "Feb")
	salesID := insertMetric(t, store, ctx, modelID, revID, "sales", "", true)
	prevID := insertCalc(t, store, ctx, modelID, revID, "prev", "PREVIOUS(sales)")
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, []string{salesID, prevID})
	insertFact(t, store, ctx, modelID, revID, salesID, fmt.Sprintf(`{"%s":"2026-01"}`, monthID), 100)
	insertFact(t, store, ctx, modelID, revID, salesID, fmt.Sprintf(`{"%s":"2026-02"}`, monthID), 120)

	sched := calculation.NewScheduler(logger.New("calc-test"), store, nil)
	_ = sched.RecalcAffected(ctx, modelID, revID, []string{salesID})
	if _, ok := calcRow(t, store, ctx, modelID, revID, prevID, map[string]string{monthID: "2026-02"}); ok {
		t.Error("a standard dimension named month must not act as time: no value may be persisted")
	}
	var status, errMsg string
	if err := store.Pool().QueryRow(ctx,
		`SELECT status::text, COALESCE(error,'') FROM runtime.metric_partition_state WHERE metric_id=$1::uuid`, prevID).Scan(&status, &errMsg); err != nil {
		t.Fatalf("partition state: %v", err)
	}
	if status != "error" || !strings.Contains(errMsg, "TIME_CONTEXT_REQUIRED") {
		t.Errorf("want error state with TIME_CONTEXT_REQUIRED, got %s %q", status, errMsg)
	}
}

func insertDimRev(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revID, name string) string {
	t.Helper()
	var id string
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, $3) RETURNING id::text
	`, modelID, revID, name).Scan(&id); err != nil {
		t.Fatalf("insert dim %s: %v", name, err)
	}
	return id
}

// TestTimeSummaryOnOrdinaryMetrics: a plain (non-time-series) formula on a
// time dimension keeps its leaves but reduces time by time_summary, and a
// formula-rule ratio still totals as the formula at the aggregate — its
// operands themselves reduced by their own time summaries.
func TestTimeSummaryOnOrdinaryMetrics(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000a5"
	revID := "00000000-0000-0000-0001-0000000000a5"

	monthID := insertTimeDim(t, store, ctx, modelID, revID, "month")
	months := insertMonths(t, store, ctx, monthID, 4)
	balID := insertMetric(t, store, ctx, modelID, revID, "bal", "", true)
	if _, err := store.Pool().Exec(ctx, `UPDATE model.metric_def SET time_summary='last' WHERE id=$1::uuid`, balID); err != nil {
		t.Fatal(err)
	}
	bal2ID := insertCalc(t, store, ctx, modelID, revID, "bal2", "bal * 2")
	if _, err := store.Pool().Exec(ctx, `UPDATE model.metric_def SET time_summary='last' WHERE id=$1::uuid`, bal2ID); err != nil {
		t.Fatal(err)
	}
	pctID := insertCalc(t, store, ctx, modelID, revID, "pct", "bal2 / bal * 100")
	if _, err := store.Pool().Exec(ctx, `UPDATE model.metric_def SET agg_rule='formula' WHERE id=$1::uuid`, pctID); err != nil {
		t.Fatal(err)
	}
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, []string{balID, bal2ID, pctID})
	for i, v := range []float64{10, 20, 30, 40} {
		insertFact(t, store, ctx, modelID, revID, balID, fmt.Sprintf(`{"%s":"%s"}`, monthID, months[i]), v)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{balID})

	if got, _ := calcRow(t, store, ctx, modelID, revID, bal2ID, map[string]string{monthID: months[1]}); math.Abs(got-40) > 1e-6 {
		t.Errorf("bal2 @ Feb: got %v want 40", got)
	}
	if got, ok := calcRow(t, store, ctx, modelID, revID, bal2ID, nil); !ok || math.Abs(got-80) > 1e-6 {
		t.Errorf("bal2 total with time_summary=last: got %v ok=%v want 80", got, ok)
	}
	// formula at the aggregate: bal2 (last = 80) / bal (last = 40) * 100.
	if got, ok := calcRow(t, store, ctx, modelID, revID, pctID, nil); !ok || math.Abs(got-200) > 1e-6 {
		t.Errorf("pct total (formula at aggregate over last balances): got %v ok=%v want 200", got, ok)
	}
	// time_summary 'none' writes no total at all.
	if _, err := store.Pool().Exec(ctx, `UPDATE model.metric_def SET time_summary='none' WHERE id=$1::uuid`, bal2ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `DELETE FROM runtime.calc_result WHERE metric_id=$1::uuid`, bal2ID); err != nil {
		t.Fatal(err)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{balID})
	if _, ok := calcRow(t, store, ctx, modelID, revID, bal2ID, nil); ok {
		t.Error("time_summary=none must persist no aggregate row")
	}
	if got, ok := calcRow(t, store, ctx, modelID, revID, bal2ID, map[string]string{monthID: months[3]}); !ok || math.Abs(got-80) > 1e-6 {
		t.Errorf("leaves must still be written under time_summary=none: %v ok=%v", got, ok)
	}
}

// TestTimeHierarchyAggregatePeriods: Q1..Q4 under H1/H2 under FY26. Time
// functions move along the quarters; an aggregate period's row is its
// quarters reduced by the metric's time summary (formula rules re-evaluate
// at the aggregate, with each operand reduced by its own summary).
func TestTimeHierarchyAggregatePeriods(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000a6"
	revID := "00000000-0000-0000-0001-0000000000a6"

	var periodID string
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid, $2::uuid, 'period', 'time', 'quarter', 1) RETURNING id::text`, modelID, revID).Scan(&periodID); err != nil {
		t.Fatal(err)
	}
	tx, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	add := func(code, parent, start, end string) {
		var pid *string
		if parent != "" {
			p := ids[parent]
			pid = &p
		}
		var id string
		if start == "" {
			err = tx.QueryRow(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid, $2, $2, $3::uuid) RETURNING id::text`,
				periodID, code, pid).Scan(&id)
		} else {
			err = tx.QueryRow(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, period_start, period_end, time_index)
				VALUES ($1::uuid, $2, $2, $3::uuid, $4::date, $5::date, 0) RETURNING id::text`, periodID, code, pid, start, end).Scan(&id)
		}
		if err != nil {
			t.Fatalf("add %s: %v", code, err)
		}
		ids[code] = id
	}
	add("FY26", "", "", "")
	add("H1", "FY26", "", "")
	add("H2", "FY26", "", "")
	add("Q1", "H1", "2026-01-01", "2026-03-31")
	add("Q2", "H1", "2026-04-01", "2026-06-30")
	add("Q3", "H2", "2026-07-01", "2026-09-30")
	add("Q4", "H2", "2026-10-01", "2026-12-31")
	if err := timedim.ValidateAndReindex(ctx, tx, periodID); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	salesID := insertMetric(t, store, ctx, modelID, revID, "sales", "", true)
	cumID := insertCalc(t, store, ctx, modelID, revID, "cum", "CUMULATE(sales)")
	dblID := insertCalc(t, store, ctx, modelID, revID, "dbl", "sales * 2")
	pctID := insertCalc(t, store, ctx, modelID, revID, "pct", "dbl / sales * 100")
	for id, ts := range map[string]string{cumID: "last", dblID: "sum"} {
		if _, err := store.Pool().Exec(ctx, `UPDATE model.metric_def SET time_summary=$2 WHERE id=$1::uuid`, id, ts); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Pool().Exec(ctx, `UPDATE model.metric_def SET agg_rule='formula' WHERE id=$1::uuid`, pctID); err != nil {
		t.Fatal(err)
	}
	insertGridSetup(t, store, ctx, modelID, []string{periodID}, []string{salesID, cumID, dblID, pctID})
	for i, q := range []string{"Q1", "Q2", "Q3", "Q4"} {
		insertFact(t, store, ctx, modelID, revID, salesID, fmt.Sprintf(`{"%s":"%s"}`, periodID, q), float64(100*(i+1)))
	}
	runRecalc(t, store, ctx, modelID, revID, []string{salesID})

	check := func(metricID, code string, want float64) {
		t.Helper()
		got, ok := calcRow(t, store, ctx, modelID, revID, metricID, map[string]string{periodID: code})
		if !ok || math.Abs(got-want) > 1e-6 {
			t.Errorf("%s @ %s: got %v ok=%v, want %v", metricID, code, got, ok, want)
		}
	}
	// CUMULATE along the leaves: 100, 300, 600, 1000.
	check(cumID, "Q2", 300)
	check(cumID, "Q4", 1000)
	// Aggregate periods of a time-series metric reduce by its summary (last).
	check(cumID, "H1", 300)
	check(cumID, "H2", 1000)
	check(cumID, "FY26", 1000)
	// A plain metric with summary 'sum': H1 = 2*(100+200).
	check(dblID, "H1", 600)
	check(dblID, "FY26", 2000)
	// Formula rule at an aggregate period: dbl(H1)/sales(H1)*100, each operand reduced by its own summary.
	check(pctID, "H1", 200)
	check(pctID, "FY26", 200)
	// The grand total of the time-series metric is still the time summary over the leaves.
	if got, _ := calcRow(t, store, ctx, modelID, revID, cumID, nil); math.Abs(got-1000) > 1e-6 {
		t.Errorf("cum total: %v", got)
	}
}
