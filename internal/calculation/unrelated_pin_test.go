package calculation_test

import (
	"context"
	"testing"
)

// A plain reference ignores a dimension of the cell its source neither has
// nor relates to. The scheduler resolved it pinned at a parent and rolled the
// source up once per child: a formula-rule metric on [department, cost]
// reading rate on [cost] showed rate's total twice over at All Departments.
func TestPlainReferenceIgnoresAnUnrelatedParentPin(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	const (
		model = "00000000-0000-0000-0000-0000000000e8"
		rev   = "00000000-0000-0000-0001-0000000000e8"
	)
	deptID := insertDim(t, store, ctx, model, "department")
	all := addMember(t, store, ctx, deptID, "ALL", "", nil)
	addMember(t, store, ctx, deptID, "X", all, nil)
	addMember(t, store, ctx, deptID, "Y", all, nil)
	costID := insertDim(t, store, ctx, model, "cost")
	insertMember(t, store, ctx, costID, "A", "A")
	insertMember(t, store, ctx, costID, "B", "B")

	rateID := insertMetric(t, store, ctx, model, rev, "rate", "", true)
	noteID := insertMetricAgg(t, store, ctx, model, rev, "note", "rate * 1", false, "formula")
	insertDep(t, store, ctx, noteID, rateID)
	insertGridSetup(t, store, ctx, model, []string{costID}, []string{rateID})
	insertGridSetup(t, store, ctx, model, []string{deptID, costID}, []string{noteID})
	insertFact(t, store, ctx, model, rev, rateID, `{"`+costID+`": "A"}`, 2.5)
	insertFact(t, store, ctx, model, rev, rateID, `{"`+costID+`": "B"}`, 6)
	runRecalc(t, store, ctx, model, rev, []string{rateID})

	c := rowChecker{t, store, ctx, model, rev, map[string]string{noteID: "note"}}
	c.want(noteID, pins(deptID, "X", costID, "A"), 2.5)
	c.want(noteID, pins(deptID, "ALL"), 8.5) // rate's total, read once
}
