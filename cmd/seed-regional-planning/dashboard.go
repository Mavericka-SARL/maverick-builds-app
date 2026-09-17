package main

type dashboardResult struct {
	dashboardID string
}

// buildDashboard lays out a business-user-facing dashboard: KPI tiles, a
// chart, a grid, a form, and a manual-trigger button for the workflow —
// with explicit, non-overlapping pos_x/pos_y/size_w/size_h so it reads as
// an actual laid-out page rather than a stack of default-positioned boxes.
func buildDashboard(dev *api, d dims, m metrics, g grids, frm formResult, wf workflowResult) dashboardResult {
	step("creating dashboard \"Regional Finance Overview\"")
	dash := dev.call("POST", "/api/developer/dashboards", map[string]any{
		"name": "Regional Finance Overview",
		"tags": []string{"finance", "regional"},
	})
	dashID := id(dash)

	addWidget(dev, dashID, widget{
		WidgetType: "text", Content: strPtr("Regional Finance Overview — FY2026"),
		PosX: 0, PosY: 0, SizeW: 1180, SizeH: 40,
	})
	addWidget(dev, dashID, widget{
		WidgetType: "metric_kpi", RefID: strPtr(m.totalCompanyCost),
		PosX: 0, PosY: 50, SizeW: 280, SizeH: 100,
	})
	addWidget(dev, dashID, widget{
		WidgetType: "metric_kpi", RefID: strPtr(m.regionalTotalCost),
		PosX: 300, PosY: 50, SizeW: 280, SizeH: 100,
	})
	addWidget(dev, dashID, widget{
		WidgetType: "chart", RefID: strPtr(g.regionalRollupID),
		PosX: 0, PosY: 170, SizeW: 580, SizeH: 320,
		WidgetProps: map[string]any{
			"chart": map[string]any{
				"chart_type":   "bar",
				"dimension_id": d.regionID,
				"metric_ids":   []string{m.regionalTotalCost},
			},
		},
	})
	addWidget(dev, dashID, widget{
		WidgetType: "grid", RefID: strPtr(g.departmentBudgetID),
		PosX: 600, PosY: 170, SizeW: 580, SizeH: 320,
	})
	addWidget(dev, dashID, widget{
		WidgetType: "form", RefID: strPtr(frm.formID),
		PosX: 0, PosY: 510, SizeW: 580, SizeH: 300,
	})
	addWidget(dev, dashID, widget{
		WidgetType: "automation_button", RefID: strPtr(wf.automationRuleID),
		Content: strPtr("Start Travel Expense Approval"),
		PosX:    600, PosY: 510, SizeW: 280, SizeH: 60,
	})

	return dashboardResult{dashboardID: dashID}
}

// grantFinanceReviewDashboardAccess wires the "Finance Review" named role
// (created in buildUsersAndAccess, before any dashboard existed) to the
// dashboard buildDashboard just created. Without this, Alex's business_role
// membership makes every dashboard's chart widgets deny him access — the
// chart-data handler's access check is "if a member of any business_role,
// restrict to that role's explicit dashboard grants," not additive, so an
// empty grant list means "sees nothing," silently contradicting
// buildUsersAndAccess's own "no rules, so full default access everywhere"
// comment. Confirmed live (2026-08-10): every chart-data widget request
// 403'd "dashboard not accessible" for Alex despite him being the tenant's
// own business_admin — found while manually browsing the seeded demo.
func grantFinanceReviewDashboardAccess(admin *api, boot bootstrapResult, roleID, dashboardID string) {
	step("granting \"Finance Review\" role access to the Regional Finance Overview dashboard")
	alex := admin.as("admin-created-alex.chen@meridian-industries.demo").withApp(boot.appID)
	alex.call("PUT", "/api/business-admin/roles/"+roleID+"/dashboards", map[string]any{"dashboard_ids": []string{dashboardID}})
}

type widget struct {
	WidgetType   string
	RefID        *string
	Content      *string
	PosX, PosY   int
	SizeW, SizeH int
	WidgetProps  map[string]any
}

func addWidget(dev *api, dashboardID string, w widget) {
	body := map[string]any{
		"widget_type": w.WidgetType,
		"pos_x":       w.PosX, "pos_y": w.PosY,
		"size_w": w.SizeW, "size_h": w.SizeH,
	}
	if w.RefID != nil {
		body["ref_id"] = *w.RefID
	}
	if w.Content != nil {
		body["content"] = *w.Content
	}
	if w.WidgetProps != nil {
		body["widget_props"] = w.WidgetProps
	}
	dev.call("POST", "/api/developer/dashboards/"+dashboardID+"/widgets", body)
}

func strPtr(s string) *string { return &s }
