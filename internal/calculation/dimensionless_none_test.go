package calculation_test

import (
	"context"
	"testing"
)

// A metric with no dimensions has one value, its '{}' row. Rule none ("no
// total above the leaves") dropped that row, so the HR model's Net HC
// Change (=ending_hc - setup_current_employees on a dimensionless grid)
// had no value at all, while a dimensionless input with rule none showed.
func TestDimensionlessRuleNoneCalcKeepsItsValue(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000e7"
	revID := "00000000-0000-0000-0001-0000000000e7"

	hcID := insertMetricAgg(t, store, ctx, modelID, revID, "current_employees", "", true, "none")
	netID := insertMetricAgg(t, store, ctx, modelID, revID, "net_change", "104 - current_employees", false, "none")
	insertDep(t, store, ctx, netID, hcID)
	insertFact(t, store, ctx, modelID, revID, hcID, "{}", 100)
	runRecalc(t, store, ctx, modelID, revID, []string{hcID})

	rowChecker{t, store, ctx, modelID, revID, map[string]string{netID: "net_change"}}.want(netID, map[string]string{}, 4)
}
