package main

// seedFacts writes realistic, distinguishable input values for Jan-Mar
// across all 5 departments (covering all 3 regions) via POST /api/cells —
// the same writeback endpoint the planning grid UI uses. Each write
// synchronously triggers calculation.Scheduler.RecalcAffected server-side
// (see internal/gateway/handler.go's cells handler), so
// total_department_cost/budget_variance_pct/regional_total_cost/
// total_company_cost's '{}' aggregates are all live by the time this
// returns — no separate recalc step needed.
func seedFacts(dev *api, d dims, m metrics) {
	depts := []string{"NA_SALES", "NA_ENG", "EU_SALES", "EU_ENG", "APAC_SALES"}
	months := []string{"JAN", "FEB", "MAR"}

	written := 0
	for di, dept := range depts {
		for mi, month := range months {
			headcount := 80000.0 + float64(di)*5000 + float64(mi)*1000
			travel := 3000.0 + float64(di)*300 + float64(mi)*100
			software := 1500.0 + float64(di)*150
			target := headcount * 1.08 // budget_target set 8% above headcount so budget_variance_pct is meaningfully non-zero

			dims := map[string]string{d.departmentID: dept, d.periodID: month}
			writeCell(dev, m.headcountCost, dims, headcount)
			writeCell(dev, m.travelCost, dims, travel)
			writeCell(dev, m.softwareCost, dims, software)
			writeCell(dev, m.budgetTarget, dims, target)
			written += 4
		}
	}
	step("wrote %d input facts across %d departments x %d months", written, len(depts), len(months))
}

func writeCell(dev *api, metricID string, dimCodes map[string]string, value float64) {
	dev.call("POST", "/api/cells", map[string]any{
		"model_id":    dev.modelID,
		"metric_id":   metricID,
		"revision_id": dev.revisionID,
		"dim_codes":   dimCodes,
		"value":       value,
	})
}
