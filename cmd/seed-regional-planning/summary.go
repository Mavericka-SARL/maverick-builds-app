package main

import "fmt"

func summary(boot bootstrapResult, d dims, m metrics, g grids, frm formResult, wf workflowResult, u usersResult, dash dashboardResult) {
	fmt.Printf(`
Regional Expense Planning demo built.

  customer_id:   %s
  workspace_id:  %s
  application_id: %s
  model_id:      %s
  revision_id:   %s (%q)

  dimensions:    region=%s department=%s(child of region) period=%s
  grids:         Department Budget=%s  Regional Rollup=%s  Company Overview=%s
  form:          Travel Expense Request=%s  mapping=%s
  workflow:      Travel Expense Approval=%s  automation_rule=%s
  dashboard:     Regional Finance Overview=%s

  users:
    Alex Chen (business_admin, Finance Review member) = %s
    Priya Patel (business_user, write NA_*/read EU_*/hidden APAC_*) = %s
    Diego Ramirez (business_user, write EU_*/read NA_*/hidden APAC_*) = %s

  personas to switch to (X-Dev-User):
    admin-created-sam.developer@meridian-industries.demo   (developer)
    admin-created-alex.chen@meridian-industries.demo       (business_admin)
    admin-created-priya.patel@meridian-industries.demo     (business_user)
    admin-created-diego.ramirez@meridian-industries.demo   (business_user)
`,
		boot.customerID, boot.workspaceID, boot.appID, boot.modelID, boot.revisionID, revisionName,
		d.regionID, d.departmentID, d.periodID,
		g.departmentBudgetID, g.regionalRollupID, g.companyOverviewID,
		frm.formID, frm.mappingID,
		wf.defID, wf.automationRuleID,
		dash.dashboardID,
		u.alexID, u.priyaID, u.diegoID,
	)
}
