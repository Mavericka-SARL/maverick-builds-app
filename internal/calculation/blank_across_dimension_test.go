package calculation_test

import (
	"context"
	"testing"
)

// TestReadAcrossADimensionWithNothingRecordedIsBlank: a metric read across a
// dimension (actual on [GL, month] read from a [month] grid) where nothing
// was entered under the cell is blank, as a leaf cell nobody entered is:
// ISBLANK is TRUE and it equals "", while arithmetic still reads 0. A month
// with a recorded value under it is not blank, a recorded 0 included.
func TestReadAcrossADimensionWithNothingRecordedIsBlank(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000f5"
	revID := "00000000-0000-0000-0001-0000000000f5"

	glID := insertDimRev(t, store, ctx, modelID, revID, "gl")
	all := addMember(t, store, ctx, glID, "ALLGL", "", nil)
	addMember(t, store, ctx, glID, "G1", all, nil)
	addMember(t, store, ctx, glID, "G2", all, nil)
	monthID := insertTimeDim(t, store, ctx, modelID, revID, "month")
	months := insertMonths(t, store, ctx, monthID, 4)

	actualID := insertMetric(t, store, ctx, modelID, revID, "actual", "", true)
	totalID := insertCalc(t, store, ctx, modelID, revID, "total", "actual")
	flagID := insertCalc(t, store, ctx, modelID, revID, "flag", "IF(ISBLANK(actual), -1, 1)")
	emptyID := insertCalc(t, store, ctx, modelID, revID, "empty", `IF(actual = "", -1, 1)`)
	plusID := insertCalc(t, store, ctx, modelID, revID, "plus", "actual + 5")
	leafFlagID := insertCalc(t, store, ctx, modelID, revID, "leaf_flag", "IF(ISBLANK(actual), -1, 1)")
	insertGridSetup(t, store, ctx, modelID, []string{glID, monthID}, []string{actualID, leafFlagID})
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, []string{totalID, flagID, emptyID, plusID})

	insertFact(t, store, ctx, modelID, revID, actualID, factJSON(glID, "G1", monthID, months[0]), 10)
	insertFact(t, store, ctx, modelID, revID, actualID, factJSON(glID, "G2", monthID, months[1]), 20)
	insertFact(t, store, ctx, modelID, revID, actualID, factJSON(glID, "G1", monthID, months[3]), 0) // a recorded 0
	runRecalc(t, store, ctx, modelID, revID, []string{actualID})

	c := rowChecker{t, store, ctx, modelID, revID, map[string]string{
		totalID: "total", flagID: "flag", emptyID: "empty", plusID: "plus", leafFlagID: "leaf_flag"}}
	jan, feb, mar, apr := pins(monthID, months[0]), pins(monthID, months[1]), pins(monthID, months[2]), pins(monthID, months[3])

	// Nothing recorded under March: blank to ISBLANK and "", 0 in arithmetic.
	c.want(flagID, mar, -1)
	c.want(emptyID, mar, -1)
	c.want(totalID, mar, 0)
	c.want(plusID, mar, 5)
	// As at a leaf nobody entered.
	c.want(leafFlagID, pins(glID, "G2", monthID, months[0]), -1)

	c.want(totalID, jan, 10)
	c.want(totalID, feb, 20)
	for _, at := range []map[string]string{jan, feb, apr} {
		c.want(flagID, at, 1)
		c.want(emptyID, at, 1)
	}
}
