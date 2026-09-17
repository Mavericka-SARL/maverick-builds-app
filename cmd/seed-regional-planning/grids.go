package main

import "fmt"

type grids struct {
	departmentBudgetID, regionalRollupID, companyOverviewID string
}

// buildGrids creates 3 grids whose metric assignments deliberately exercise
// both branches of cross-dimension formula resolution: Regional Rollup's
// metric references a metric whose own grid dimension (department) IS a
// declared structural child of one of Regional Rollup's own dims (region);
// Company Overview's metric references a metric dimensioned by region,
// which Company Overview doesn't have AT ALL.
func buildGrids(dev *api, d dims, m metrics) grids {
	var g grids

	step("creating grid \"Department Budget\" (dims: department, period)")
	g.departmentBudgetID = createGrid(dev, "Department Budget")
	assignDimension(dev, g.departmentBudgetID, d.departmentID)
	assignDimension(dev, g.departmentBudgetID, d.periodID)
	for _, metricID := range []string{
		m.headcountCost, m.travelCost, m.softwareCost, m.budgetTarget,
		m.totalDepartmentCost, m.budgetVariancePct,
	} {
		assignMetric(dev, g.departmentBudgetID, metricID)
	}

	step("creating grid \"Regional Rollup\" (dims: region, period)")
	g.regionalRollupID = createGrid(dev, "Regional Rollup")
	assignDimension(dev, g.regionalRollupID, d.regionID)
	assignDimension(dev, g.regionalRollupID, d.periodID)
	assignMetric(dev, g.regionalRollupID, m.regionalTotalCost)

	step("creating grid \"Company Overview\" (dims: period only — no region/department)")
	g.companyOverviewID = createGrid(dev, "Company Overview")
	assignDimension(dev, g.companyOverviewID, d.periodID)
	assignMetric(dev, g.companyOverviewID, m.totalCompanyCost)

	return g
}

func createGrid(dev *api, name string) string {
	return id(dev.call("POST", "/api/developer/grids", map[string]any{
		"name": name, "revision_id": dev.revisionID,
	}))
}

func assignDimension(dev *api, gridID, dimensionID string) {
	dev.call("POST", fmt.Sprintf("/api/developer/grids/%s/dimensions/%s", gridID, dimensionID), nil)
}

func assignMetric(dev *api, gridID, metricID string) {
	dev.call("POST", fmt.Sprintf("/api/developer/grids/%s/metrics/%s", gridID, metricID), nil)
}
