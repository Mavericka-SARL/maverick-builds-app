package main

import "fmt"

type dims struct {
	regionID, departmentID, periodID string
	// code -> dimension_member id, for wiring parent_member_id and access rules
	region     map[string]string
	department map[string]string
	period     map[string]string
}

// buildDimensions creates two different kinds of "multilevel" dimension,
// both entirely through POST /api/developer/dimensions[/{id}/members]:
//
//   - region -> department is a CROSS-dimension parent/child pair
//     (department declares parent_dimension_id=region; each department
//     member's parent_member_id points at its region member).
//   - period is a SAME-dimension hierarchy (quarters as roots, months as
//     children of their quarter, all within the one "period" dimension).
func buildDimensions(dev *api) dims {
	d := dims{
		region:     map[string]string{},
		department: map[string]string{},
		period:     map[string]string{},
	}

	step("creating dimension \"region\"")
	d.regionID = createDimension(dev, "region", "")
	for _, m := range []struct{ code, label string }{
		{"NA", "North America"}, {"EU", "Europe"}, {"APAC", "Asia Pacific"},
	} {
		d.region[m.code] = createMember(dev, d.regionID, m.code, m.label, "")
	}

	step("creating dimension \"department\" (cross-dimension child of region)")
	d.departmentID = createDimension(dev, "department", d.regionID)
	deptMembers := []struct{ code, label, parentRegion string }{
		{"NA_SALES", "NA Sales", "NA"}, {"NA_ENG", "NA Engineering", "NA"},
		{"EU_SALES", "EU Sales", "EU"}, {"EU_ENG", "EU Engineering", "EU"},
		{"APAC_SALES", "APAC Sales", "APAC"},
	}
	for _, m := range deptMembers {
		d.department[m.code] = createMember(dev, d.departmentID, m.code, m.label, d.region[m.parentRegion])
	}

	step("creating dimension \"period\" (same-dimension quarter/month hierarchy)")
	d.periodID = createDimension(dev, "period", "")
	quarters := []string{"Q1", "Q2", "Q3", "Q4"}
	for _, q := range quarters {
		d.period[q] = createMember(dev, d.periodID, q, q, "")
	}
	months := []struct{ code, label, quarter string }{
		{"JAN", "January", "Q1"}, {"FEB", "February", "Q1"}, {"MAR", "March", "Q1"},
		{"APR", "April", "Q2"}, {"MAY", "May", "Q2"}, {"JUN", "June", "Q2"},
		{"JUL", "July", "Q3"}, {"AUG", "August", "Q3"}, {"SEP", "September", "Q3"},
		{"OCT", "October", "Q4"}, {"NOV", "November", "Q4"}, {"DEC", "December", "Q4"},
	}
	for _, m := range months {
		d.period[m.code] = createMember(dev, d.periodID, m.code, m.label, d.period[m.quarter])
	}

	return d
}

func createDimension(dev *api, name, parentDimensionID string) string {
	body := map[string]any{"name": name, "agg_rule": "sum", "revision_id": dev.revisionID}
	if parentDimensionID != "" {
		body["parent_dimension_id"] = parentDimensionID
	}
	return id(dev.call("POST", "/api/developer/dimensions", body))
}

func createMember(dev *api, dimensionID, code, label, parentMemberID string) string {
	body := map[string]any{"code": code, "label": label}
	if parentMemberID != "" {
		body["parent_member_id"] = parentMemberID
	}
	return id(dev.call("POST", fmt.Sprintf("/api/developer/dimensions/%s/members", dimensionID), body))
}
