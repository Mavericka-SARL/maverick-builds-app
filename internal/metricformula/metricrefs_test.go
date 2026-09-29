package metricformula

import (
	"context"
	"strings"
	"testing"
)

// TestCheckMetricNotInUse: a metric another metric reads — in its formula, in
// any case, even through a formula that no longer parses, or as a Rate
// operand — cannot be deleted; the error names the readers. A metric reading
// only itself, or nobody, can.
func TestCheckMetricNotInUse(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	metric := func(name, formulaText, agg, num, den string) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula, agg_rule, agg_numerator_metric_id, agg_denominator_metric_id)
			VALUES ($1::uuid, $2::uuid, $3, false, $4, $5, NULLIF($6,'')::uuid, NULLIF($7,'')::uuid) RETURNING id::text`,
			f.modelID, f.revID, name, formulaText, agg, num, den).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		return id
	}
	selfRef := metric("selfref", "PREVIOUS(selfref) + 1", "sum", "", "")
	shouted := metric("shouted", "OTHER * 2", "sum", "", "")
	metric("ratio", "1", "rate", f.flow, f.revenue)

	for _, c := range []struct {
		name, id string
		readers  []string // nil: deletable
	}{
		{"revenue", f.revenue, []string{"ratio (its Rate total)", "selfy"}},
		{"other", f.other, []string{"shouted"}},
		{"flow", f.flow, []string{"closing", "ratio (its Rate total)"}},
		{"opening", f.opening, []string{"closing"}},
		{"closing", f.closing, []string{"opening"}},
		{"selfy", f.selfy, nil},
		{"selfref", selfRef, nil},
		{"shouted", shouted, nil},
	} {
		err := CheckMetricNotInUse(ctx, f.pool, c.id)
		if c.readers == nil {
			if err != nil {
				t.Errorf("%s: want deletable, got %v", c.name, err)
			}
			continue
		}
		if codeOf(err) != CodeMetricInUse {
			t.Errorf("%s: want METRIC_IN_USE, got %v", c.name, err)
			continue
		}
		for _, r := range c.readers {
			if !strings.Contains(err.Error(), r) {
				t.Errorf("%s: the error %q does not name %s", c.name, err, r)
			}
		}
	}

	// A formula that no longer parses still protects what it names.
	metric("broken", "selfy +", "sum", "", "")
	if err := CheckMetricNotInUse(ctx, f.pool, f.selfy); codeOf(err) != CodeMetricInUse || !strings.Contains(err.Error(), "broken") {
		t.Errorf("selfy read by an unparsable formula: want METRIC_IN_USE naming broken, got %v", err)
	}
}

func TestFormulaReadsMetric(t *testing.T) {
	for text, want := range map[string]bool{
		`revenue * 2`:                     true,
		`REVENUE * 2`:                     true,
		`{revenue} - cost`:                true,
		`LOOKUP(revenue, region, "DE")`:   true,
		`SUMIFS(revenue, region, "*")`:    true,
		`YEARVALUE(revenue)`:              true,
		`LAG(revenue, 1, 0)`:              true,
		`revenue_2026 * 2`:                false,
		`"revenue" & region`:              false,
		`MOVINGSUM(cost, -2, 0, AVERAGE)`: false,
		`region.revenue * 2`:              false,
		`revenue +`:                       true, // unparsable: matched as a word
		`revenue_x +`:                     false,
	} {
		if got := FormulaReadsMetric(text, "revenue"); got != want {
			t.Errorf("%s: reads revenue = %v, want %v", text, got, want)
		}
	}
}
