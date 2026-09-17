package main

type metrics struct {
	headcountCost, travelCost, softwareCost, budgetTarget string // input
	totalDepartmentCost, budgetVariancePct                string // calc, department grid
	regionalTotalCost                                     string // calc, region grid — references totalDepartmentCost (different dims)
	totalCompanyCost                                      string // calc, company grid — references regionalTotalCost (different dims, region entirely absent)
}

// buildMetrics creates every metric in strict dependency order: server-side
// formula validation (developerMetrics) requires every referenced name to
// already exist in the model, and calc_dependency edges are derived from
// whatever exists at insert time — so a metric must be created only after
// everything its formula references.
func buildMetrics(dev *api, d dims) metrics {
	var m metrics

	step("creating input metrics: headcount_cost, travel_cost, software_cost, budget_target")
	m.headcountCost = createMetric(dev, "headcount_cost", true, "", "currency")
	m.travelCost = createMetric(dev, "travel_cost", true, "", "currency")
	m.softwareCost = createMetric(dev, "software_cost", true, "", "currency")
	m.budgetTarget = createMetric(dev, "budget_target", true, "", "currency")

	step("creating total_department_cost (sum of the 3 cost inputs)")
	m.totalDepartmentCost = createMetric(dev, "total_department_cost", false,
		"=headcount_cost+travel_cost+software_cost", "currency")

	step("creating budget_variance_pct (complex formula: ROUND + division + %% delta)")
	m.budgetVariancePct = createMetric(dev, "budget_variance_pct", false,
		"=ROUND((total_department_cost-budget_target)/budget_target*100,1)", "percentage")

	step("creating regional_total_cost — references total_department_cost, whose own grid is dimensioned by department (a declared child of region)")
	m.regionalTotalCost = createMetric(dev, "regional_total_cost", false, "=total_department_cost", "currency")

	step("creating total_company_cost — references regional_total_cost, whose own grid is dimensioned by region, entirely absent from this metric's grid")
	m.totalCompanyCost = createMetric(dev, "total_company_cost", false, "=regional_total_cost", "currency")

	return m
}

func createMetric(dev *api, name string, isInput bool, formula, format string) string {
	body := map[string]any{
		"name":        name,
		"is_input":    isInput,
		"revision_id": dev.revisionID,
		"format":      format,
	}
	if !isInput {
		body["formula"] = formula
	}
	return id(dev.call("POST", "/api/developer/metrics", body))
}
