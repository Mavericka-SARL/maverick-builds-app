// Package salesdemo builds a small sales-planning model — three dimensions of
// three levels each, and metrics covering every aggregation rule the engine
// implements.
//
// It exists so the model has one definition rather than two. The integration
// test in internal/gateway and the cmd/seed-sales-planning binary both build
// it, and a demo whose test-shape and seeded-shape had drifted apart would be
// worse than no demo: the tests would keep passing against a model nobody has.
//
// Everything goes through Caller, which is whatever HTTP the caller already
// has — a dev-persona client in tests, a bearer-token one against a real
// deployment. Nothing here talks to a database, because a model built any way
// other than through the API is a model whose API was never exercised.
package salesdemo

import "fmt"

// Caller performs one authenticated API request and returns the decoded body.
type Caller func(method, path string, body any) (map[string]any, error)

// Model is what Build produced: the ids a caller needs to write facts, wire a
// form, or open the grid.
type Model struct {
	GeoDim, ProdDim, PeriodDim string
	GridID                     string

	// Member code -> member id, per dimension.
	Geo, Prod, Period map[string]string
	// Metric name -> metric id.
	Metric map[string]string
}

// MetricNames is every metric Build creates, in the order a grid should show
// them: inputs first, then what is derived from them.
var MetricNames = []string{
	"units", "revenue", "cost", "target",
	"margin", "margin_pct", "attainment_pct", "avg_price",
}

// Build creates the dimensions, metrics and grid in revisionID.
//
// Three levels per dimension is deliberate. A two-level hierarchy hides a
// whole class of bug, where a rollup is correct at the root and wrong in the
// middle — nobody notices until they open a region.
func Build(call Caller, revisionID string) (*Model, error) {
	m := &Model{
		Geo: map[string]string{}, Prod: map[string]string{}, Period: map[string]string{},
		Metric: map[string]string{},
	}

	id := func(res map[string]any, err error) (string, error) {
		if err != nil {
			return "", err
		}
		v, _ := res["id"].(string)
		if v == "" {
			return "", fmt.Errorf("response carried no id: %v", res)
		}
		return v, nil
	}

	dimension := func(name string) (string, error) {
		return id(call("POST", "/api/developer/dimensions", map[string]any{"name": name, "revision_id": revisionID}))
	}
	member := func(dimID, code, label, parent string) (string, error) {
		body := map[string]any{"code": code, "label": label}
		if parent != "" {
			body["parent_member_id"] = parent
		}
		return id(call("POST", "/api/developer/dimensions/"+dimID+"/members", body))
	}

	// A hierarchy as (code, label, parent-code); "" parent means a root, and
	// parents are always declared before their children.
	type node struct{ code, label, parent string }
	build := func(name string, into map[string]string, nodes []node) (string, error) {
		dimID, err := dimension(name)
		if err != nil {
			return "", fmt.Errorf("create dimension %s: %w", name, err)
		}
		for _, n := range nodes {
			var parentID string
			if n.parent != "" {
				parentID = into[n.parent]
				if parentID == "" {
					return "", fmt.Errorf("%s: parent %q declared after its child %q", name, n.parent, n.code)
				}
			}
			memberID, err := member(dimID, n.code, n.label, parentID)
			if err != nil {
				return "", fmt.Errorf("create member %s/%s: %w", name, n.code, err)
			}
			into[n.code] = memberID
		}
		return dimID, nil
	}

	var err error
	if m.GeoDim, err = build("geography", m.Geo, []node{
		{"WORLD", "World", ""},
		{"EMEA", "EMEA", "WORLD"}, {"AMER", "Americas", "WORLD"},
		{"UK", "United Kingdom", "EMEA"}, {"DE", "Germany", "EMEA"},
		{"US", "United States", "AMER"}, {"CA", "Canada", "AMER"},
	}); err != nil {
		return nil, err
	}
	if m.ProdDim, err = build("product", m.Prod, []node{
		{"ALL_PROD", "All products", ""},
		{"HARDWARE", "Hardware", "ALL_PROD"}, {"SOFTWARE", "Software", "ALL_PROD"},
		{"LAPTOP", "Laptop", "HARDWARE"}, {"MONITOR", "Monitor", "HARDWARE"},
		{"LICENSE", "Licence", "SOFTWARE"}, {"SUPPORT", "Support", "SOFTWARE"},
	}); err != nil {
		return nil, err
	}
	if m.PeriodDim, err = build("period", m.Period, []node{
		{"FY26", "FY26", ""},
		{"H1", "H1", "FY26"}, {"H2", "H2", "FY26"},
		{"Q1", "Q1", "H1"}, {"Q2", "Q2", "H1"},
		{"Q3", "Q3", "H2"}, {"Q4", "Q4", "H2"},
	}); err != nil {
		return nil, err
	}

	metric := func(name, formula string, isInput bool, agg string, extra map[string]any) error {
		body := map[string]any{
			"name": name, "is_input": isInput, "formula": formula,
			"revision_id": revisionID, "agg_rule": agg, "format": "number",
		}
		for k, v := range extra {
			body[k] = v
		}
		mid, err := id(call("POST", "/api/developer/metrics", body))
		if err != nil {
			return fmt.Errorf("create metric %s: %w", name, err)
		}
		m.Metric[name] = mid
		return nil
	}

	for _, in := range []string{"units", "revenue", "cost", "target"} {
		if err := metric(in, "", true, "sum", nil); err != nil {
			return nil, err
		}
	}
	// Sums, because a margin at any level really is the sum of the margins
	// below it.
	if err := metric("margin", "={revenue} - {cost}", false, "sum", nil); err != nil {
		return nil, err
	}
	// Percentages. Summing these is meaningless and averaging them weights a
	// tiny country the same as a huge one, so the total is the formula run
	// against aggregated inputs.
	if err := metric("margin_pct", "={margin} / {revenue} * 100", false, "formula", nil); err != nil {
		return nil, err
	}
	if err := metric("attainment_pct", "={revenue} / {target} * 100", false, "formula", nil); err != nil {
		return nil, err
	}
	// A ratio of two other metrics — Anaplan's Ratio summary. The blended
	// price is total revenue over total units, never an average of prices.
	if err := metric("avg_price", "={revenue} / {units}", false, "rate", map[string]any{
		"agg_numerator_metric_id":   m.Metric["revenue"],
		"agg_denominator_metric_id": m.Metric["units"],
	}); err != nil {
		return nil, err
	}

	if m.GridID, err = id(call("POST", "/api/developer/grids", map[string]any{"name": "Sales Plan", "revision_id": revisionID})); err != nil {
		return nil, fmt.Errorf("create grid: %w", err)
	}
	// Grid membership is a sub-resource: the id goes in the path, not a body.
	for _, dimID := range []string{m.GeoDim, m.ProdDim, m.PeriodDim} {
		if _, err := call("POST", "/api/developer/grids/"+m.GridID+"/dimensions/"+dimID, nil); err != nil {
			return nil, fmt.Errorf("add dimension to grid: %w", err)
		}
	}
	for _, name := range MetricNames {
		if _, err := call("POST", "/api/developer/grids/"+m.GridID+"/metrics/"+m.Metric[name], nil); err != nil {
			return nil, fmt.Errorf("add metric %s to grid: %w", name, err)
		}
	}
	return m, nil
}

// Fact is one seeded number.
type Fact struct {
	Geo, Prod, Period            string
	Units, Revenue, Cost, Target float64
}

// SampleFacts is a deliberately lopsided plan: the UK sells a lot of cheap
// laptops, Germany a few expensive ones. Even numbers would let a wrong
// aggregation rule accidentally agree with the right one, which is exactly
// what a demo must not do.
func SampleFacts() []Fact {
	return []Fact{
		{"UK", "LAPTOP", "Q1", 900, 900_000, 700_000, 1_000_000},
		{"DE", "LAPTOP", "Q1", 100, 500_000, 300_000, 400_000},
		{"US", "LICENSE", "Q1", 200, 400_000, 100_000, 300_000},
		{"CA", "LICENSE", "Q2", 50, 100_000, 40_000, 150_000},
	}
}

// WriteFacts enters SampleFacts through the same cell endpoint a planner uses.
func (m *Model) WriteFacts(call Caller, modelID, revisionID string, facts []Fact) error {
	for _, f := range facts {
		for name, value := range map[string]float64{
			"units": f.Units, "revenue": f.Revenue, "cost": f.Cost, "target": f.Target,
		} {
			if _, err := call("POST", "/api/cells", map[string]any{
				"model_id": modelID, "metric_id": m.Metric[name], "revision_id": revisionID,
				"dim_codes": map[string]string{
					m.GeoDim: f.Geo, m.ProdDim: f.Prod, m.PeriodDim: f.Period,
				},
				"value": value,
			}); err != nil {
				return fmt.Errorf("write %s at %s/%s/%s: %w", name, f.Geo, f.Prod, f.Period, err)
			}
		}
	}
	return nil
}

// ── Time-series forecasting ─────────────────────────────────────────────────

// ForecastMetricNames is what BuildForecast adds, inputs first.
var ForecastMetricNames = []string{
	"prior_revenue", "seasonality", "pipeline",
	"growth_pct", "forecast_base", "forecast_seasonal", "forecast_capped",
	"weighted_target", "h1_revenue", "abs_variance", "cagr_pct", "rmse",
	"scenario_mid", "best_case", "worst_case", "input_total",
	"funded_flag", "off_peak_revenue", "units_whole", "batch_remainder",
	"risk_tier", "pallets_needed", "pallets_full", "revenue_k_down", "revenue_k_up",
	"scenarios_present", "quarter_weight",
}

// BuildForecast adds a quarterly forecast on top of the plan Build created.
//
// One thing shapes the whole design: the engine has no time-offset function.
// ForecastMetric is one calculated forecast metric. Each entry names the
// functions it is there to exercise, so a failure points at a function
// rather than at "the forecast".
type ForecastMetric struct{ Name, Formula, Agg string }

// ForecastMetrics is the one definition of the calculated forecast metrics.
// BuildForecast creates them over HTTP; the AI-build test in internal/gateway
// proposes the same list through the assistant's own tools, so "a developer
// can write this" and "the AI Developer can write this" are checked against
// the same formulas rather than two copies that can drift.
var ForecastMetrics = []ForecastMetric{
	// Growth off the prior period. IFERROR is doing real work: the first
	// period of any series has no prior, and dividing by it is the most
	// common way a forecast model produces #DIV/0! across a whole column.
	{Name: "growth_pct", Formula: "=IFERROR(({revenue} - {prior_revenue}) / {prior_revenue} * 100, 0)", Agg: "formula"},

	{Name: "forecast_base", Formula: "={revenue} * (1 + {growth_pct} / 100)", Agg: "sum"},
	{Name: "forecast_seasonal", Formula: "=ROUND({forecast_base} * {seasonality}, 0)", Agg: "sum"},

	// A floor and a ceiling, the usual guard rails on a projection.
	{Name: "forecast_capped", Formula: "=MIN(MAX({forecast_seasonal}, 0), 2000000)", Agg: "sum"},

	// Reading the period being evaluated, phrased as a planner would: this
	// metric, weighted by when it falls.
	{Name: "weighted_target", Formula: `={target} * SWITCH(period, "Q1", 0.2, "Q2", 0.25, "Q3", 0.25, 0.3)`, Agg: "sum"},
	{Name: "h1_revenue", Formula: `=IF(OR(period = "Q1", period = "Q2"), {revenue}, 0)`, Agg: "sum"},
	{Name: "off_peak_revenue", Formula: `=IF(NOT(period = "Q4"), {revenue}, 0)`, Agg: "sum"},

	// And one that reads ONLY the period, depending on no metric at all.
	// It is here because that shape used to compute nothing: with no
	// dependency edges it was never reachable from a changed input, so it
	// saved cleanly and produced a permanently blank column. Kept in the
	// demo as a live check that it still runs.
	{Name: "quarter_weight", Formula: `=SWITCH(period, "Q1", 0.2, "Q2", 0.25, "Q3", 0.25, 0.3)`, Agg: "sum"},

	{Name: "abs_variance", Formula: "=ABS({revenue} - {target})", Agg: "sum"},
	// Compound growth over four quarters.
	{Name: "cagr_pct", Formula: "=IFERROR((POWER({revenue} / {prior_revenue}, 0.25) - 1) * 100, 0)", Agg: "formula"},
	{Name: "rmse", Formula: "=SQRT(POWER({revenue} - {target}, 2))", Agg: "sum"},

	{Name: "scenario_mid", Formula: "=AVERAGE({revenue}, {target}, {pipeline})", Agg: "sum"},
	{Name: "best_case", Formula: "=MAX({revenue}, {target}, {pipeline})", Agg: "sum"},
	{Name: "worst_case", Formula: "=MIN({revenue}, {target}, {pipeline})", Agg: "sum"},
	{Name: "input_total", Formula: "=SUM({revenue}, {cost}, {target})", Agg: "sum"},

	{Name: "funded_flag", Formula: "=IF(AND({revenue} > 0, {cost} > 0), 1, 0)", Agg: "sum"},

	{Name: "units_whole", Formula: "=INT({units})", Agg: "sum"},
	{Name: "batch_remainder", Formula: "=MOD({units}, 7)", Agg: "sum"},

	// Tiering, the other shape a forecast rule takes besides SWITCH: a
	// cascade of conditions rather than a lookup on one value.
	{Name: "risk_tier", Formula: "=IFS({growth_pct} > 15, 3, {growth_pct} > 5, 2, {growth_pct} > -100, 1)", Agg: "sum"},

	// Rounding, all four ways, because "round" means four different things
	// to a planner and picking the wrong one shifts a total.
	{Name: "pallets_needed", Formula: "=CEILING({units} / 48, 1)", Agg: "sum"},
	{Name: "pallets_full", Formula: "=FLOOR({units} / 48, 1)", Agg: "sum"},
	{Name: "revenue_k_down", Formula: "=ROUNDDOWN({revenue} / 1000, 0)", Agg: "sum"},
	{Name: "revenue_k_up", Formula: "=ROUNDUP({revenue} / 1000, 0)", Agg: "sum"},

	// How many of the three scenarios carry a number at all.
	{Name: "scenarios_present", Formula: "=COUNT({revenue}, {target}, {pipeline})", Agg: "sum"},
}

// There is no LAG, PRIOR or OFFSET among the 46 in internal/formula, so a
// formula cannot reach into the previous period. Prior-period actuals are
// therefore held as their own input metric — which is how a planner would
// model it anyway when the alternative does not exist, and is worth knowing
// before anyone tries to write =revenue[-1].
//
// What a formula CAN do is read the period it is being evaluated at: a bare
// dimension name resolves to the current member's code, which is what makes
// the seasonal and half-year rules below possible.
func BuildForecast(call Caller, revisionID string, m *Model) error {
	add := func(name, formula string, isInput bool, agg string) error {
		body := map[string]any{
			"name": name, "is_input": isInput, "formula": formula,
			"revision_id": revisionID, "agg_rule": agg, "format": "number",
		}
		res, err := call("POST", "/api/developer/metrics", body)
		if err != nil {
			return fmt.Errorf("create metric %s: %w", name, err)
		}
		id, _ := res["id"].(string)
		if id == "" {
			return fmt.Errorf("create metric %s: no id in %v", name, res)
		}
		m.Metric[name] = id
		return nil
	}

	for _, in := range []string{"prior_revenue", "seasonality", "pipeline"} {
		if err := add(in, "", true, "sum"); err != nil {
			return err
		}
	}

	for _, f := range ForecastMetrics {
		if err := add(f.Name, f.Formula, false, f.Agg); err != nil {
			return err
		}
	}

	// The forecast belongs on the same grid as the plan it forecasts.
	for _, name := range ForecastMetricNames {
		if _, err := call("POST", "/api/developer/grids/"+m.GridID+"/metrics/"+m.Metric[name], nil); err != nil {
			return fmt.Errorf("add %s to grid: %w", name, err)
		}
	}
	return nil
}

// ForecastFact is one seeded forecasting input.
type ForecastFact struct {
	Geo, Prod, Period                   string
	PriorRevenue, Seasonality, Pipeline float64
}

// SampleForecastFacts gives Q1 a prior period and Q2 none, so the
// divide-by-zero guard is exercised by real data rather than only in theory.
func SampleForecastFacts() []ForecastFact {
	return []ForecastFact{
		{"UK", "LAPTOP", "Q1", 750_000, 1.10, 1_200_000},
		{"DE", "LAPTOP", "Q1", 400_000, 0.90, 450_000},
		{"US", "LICENSE", "Q1", 320_000, 1.00, 500_000},
		{"CA", "LICENSE", "Q2", 0, 1.25, 120_000},
	}
}

// WriteForecastFacts enters them through the same cell endpoint.
func (m *Model) WriteForecastFacts(call Caller, modelID, revisionID string, facts []ForecastFact) error {
	for _, f := range facts {
		for name, value := range map[string]float64{
			"prior_revenue": f.PriorRevenue, "seasonality": f.Seasonality, "pipeline": f.Pipeline,
		} {
			if _, err := call("POST", "/api/cells", map[string]any{
				"model_id": modelID, "metric_id": m.Metric[name], "revision_id": revisionID,
				"dim_codes": map[string]string{
					m.GeoDim: f.Geo, m.ProdDim: f.Prod, m.PeriodDim: f.Period,
				},
				"value": value,
			}); err != nil {
				return fmt.Errorf("write %s at %s/%s/%s: %w", name, f.Geo, f.Prod, f.Period, err)
			}
		}
	}
	return nil
}
