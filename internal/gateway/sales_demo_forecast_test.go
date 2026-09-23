package gateway

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/salesdemo"
)

// A quarterly forecast on the sales-planning model, exercising the functions a
// forecast actually leans on — including LAG and PREVIOUS along the declared
// time dimension, which is how the previous quarter is read (there is no
// separately maintained prior-period input).
//
// Every assertion is at one intersection with known inputs, so a failure names
// a function rather than "the forecast is wrong".
func TestSalesDemoForecastFormulas(t *testing.T) {
	d := setupSalesDemo(t)

	caller := func(method, path string, body any) (map[string]any, error) {
		status, raw := d.req(method, path, d.dev, body)
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("%s %s: status %d: %s", method, path, status, raw)
		}
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		return parsed, nil
	}
	if err := salesdemo.BuildForecast(caller, d.revID, d.model); err != nil {
		t.Fatalf("build forecast: %v", err)
	}
	d.seedFacts()
	// A second quarter for UK laptops, so LAG has a real prior period to
	// read: Q2 revenue 1.08m against Q1's 900k is 20% growth. Written before
	// the forecast inputs so the last recalculation triggered by seeding has
	// seen every fact.
	if _, err := caller("POST", "/api/cells", map[string]any{
		"model_id": d.modelID, "metric_id": d.model.Metric["revenue"], "revision_id": d.revID,
		"dim_codes": map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP", d.periodDim: "Q2"},
		"value":     1_080_000,
	}); err != nil {
		t.Fatalf("seed Q2 revenue: %v", err)
	}
	if err := d.model.WriteForecastFacts(caller, d.modelID, d.revID, salesdemo.SampleForecastFacts()); err != nil {
		t.Fatalf("seed forecast inputs: %v", err)
	}

	// UK / LAPTOP / Q1, where: revenue 900k, cost 700k, target 1m, units 900,
	// seasonality 1.10, pipeline 1.2m.
	at := map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP", d.periodDim: "Q1"}
	// UK / LAPTOP / Q2: revenue 1.08m, seasonality 1.10, pipeline 1.2m, and
	// a previous quarter to grow from.
	atQ2 := map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP", d.periodDim: "Q2"}

	// growth at Q2 = (1.08m - 900k) / 900k * 100
	const growth = 20.0
	// base at Q2 = 1.08m * 1.20; seasonal = round(base * 1.10)
	const base = 1_296_000.0
	const seasonal = 1_425_600.0

	for _, c := range []struct {
		metric    string
		want      float64
		functions string
	}{
		// SWITCH on the period being evaluated.
		// target 1m x the Q1 weight of 0.2.
		{"weighted_target", 200_000, "SWITCH + dimension reference"},
		{"h1_revenue", 900_000, "IF, OR + dimension reference"},
		{"off_peak_revenue", 900_000, "IF, NOT + dimension reference"},
		{"abs_variance", 100_000, "ABS"},            // |900k - 1m|
		{"rmse", 100_000, "SQRT, POWER"},            // sqrt((900k-1m)^2)
		{"scenario_mid", 1_033_333.3333, "AVERAGE"}, // (900k + 1m + 1.2m)/3
		{"best_case", 1_200_000, "MAX over 3"},      // pipeline
		{"worst_case", 900_000, "MIN over 3"},       // revenue
		{"input_total", 2_600_000, "SUM over 3"},    // 900k + 700k + 1m
		{"funded_flag", 1, "IF, AND"},
		{"units_whole", 900, "INT"},
		{"batch_remainder", 4, "MOD"},        // 900 mod 7
		{"pallets_needed", 19, "CEILING"},    // 900/48 = 18.75
		{"pallets_full", 18, "FLOOR"},        // same, the other way
		{"revenue_k_down", 900, "ROUNDDOWN"}, // 900000/1000
		{"revenue_k_up", 900, "ROUNDUP"},
		{"scenarios_present", 3, "COUNT"},
		// Depends on no metric whatsoever — the shape that used to compute
		// nothing at all.
		{"quarter_weight", 0.2, "SWITCH with no metric reference"},
	} {
		t.Run(c.metric, func(t *testing.T) {
			d.awaitCalc(c.metric, at, c.want)
		})
	}

	// The growth chain reads the previous quarter through LAG / PREVIOUS,
	// so it is asserted at Q2, where UK laptops have one.
	for _, c := range []struct {
		metric    string
		want      float64
		functions string
	}{
		{"growth_pct", growth, "LAG + IFERROR + arithmetic"},
		{"forecast_base", base, "arithmetic on a time-series calc metric"},
		{"forecast_seasonal", seasonal, "ROUND"},
		// MIN(MAX(1.4256m, 0), 2m) — inside both guard rails.
		{"forecast_capped", seasonal, "MIN, MAX"},
		// growth is 20, so the top tier.
		{"risk_tier", 3, "IFS"},
	} {
		t.Run(c.metric+"@Q2", func(t *testing.T) {
			d.awaitCalc(c.metric, atQ2, c.want)
		})
	}

	// CAGR compounds, so it is asserted separately rather than buried in the
	// table: (1080/900)^0.25 - 1, as a percentage, read with PREVIOUS.
	t.Run("cagr_pct", func(t *testing.T) {
		d.awaitCalc("cagr_pct", atQ2, 4.6635)
	})

	// The divide-by-zero guard, on real data rather than in theory. The first
	// quarter has no previous one (LAG substitutes 0), and without IFERROR
	// this is #DIV/0! spreading through every metric downstream — at Q1 for
	// UK laptops and for CA licences, which only have a Q2.
	t.Run("no prior period yields zero growth rather than an error", func(t *testing.T) {
		d.awaitCalc("growth_pct", at, 0)
		d.awaitCalc("growth_pct", map[string]string{
			d.geoDim: "CA", d.prodDim: "LICENSE", d.periodDim: "Q2",
		}, 0)
	})
}
