package main

type workflowResult struct {
	defID string
	// automationRuleID is the "on form_submit, start this workflow"
	// trigger — the automatic path. The dashboard's automation_button
	// widget (see dashboard.go) provides the manual trigger path, so both
	// trigger styles are demonstrated.
	automationRuleID string
}

// buildWorkflow creates a 4-step approval chain (task -> approval ->
// approval -> notification) bound to the Travel Expense Request form's
// records, then publishes it and wires an automation rule so submitting
// the form automatically starts an instance. Route keys match exactly what
// internal/workflow/store.go's runtime engine reads: "next" for a task
// step's single outgoing route, "approve"/"reject" for approval steps,
// "sent" for a notification step's post-dispatch route (see
// activateNextSteps call sites) — get these wrong and the workflow stalls
// after the first step, silently.
func buildWorkflow(dev *api, boot bootstrapResult, frm formResult) workflowResult {
	step("creating workflow def \"Travel Expense Approval\"")
	created := dev.call("POST", "/api/developer/workflows?application_id="+boot.appID, map[string]any{
		"name":          "Travel Expense Approval",
		"description":   "Reviews and approves travel expense requests before they post to the budget.",
		"trigger_event": "manual",
	})
	defID := id(created)

	steps := []map[string]any{
		{
			"id": "step-review", "name": "Review Request", "type": "task",
			"instructions":   "Confirm the request has a department, period, and a valid justification.",
			"assignee_roles": []string{"business_user"},
			"routes":         map[string]string{"next": "step-mgr-approval"},
		},
		{
			"id": "step-mgr-approval", "name": "Manager Approval", "type": "approval",
			"assignee_roles": []string{"business_user"},
			"sla_hours":      48,
			"routes":         map[string]string{"approve": "step-finance-approval", "reject": "end-rejected"},
		},
		{
			"id": "step-finance-approval", "name": "Finance Approval", "type": "approval",
			"assignee_roles":   []string{"Finance Review"},
			"sla_hours":        72,
			"required_comment": true,
			"routes":           map[string]string{"approve": "step-notify", "reject": "end-rejected"},
		},
		{
			"id": "step-notify", "name": "Notify Requester", "type": "notification",
			"notification": map[string]string{
				"recipient_type": "requester",
				"subject":        "Your travel expense request has been approved",
				"message":        "Finance has approved your travel expense request and it has been posted to the budget.",
			},
			"routes": map[string]string{"sent": "end-completed"},
		},
	}

	step("attaching steps + binding to the Travel Expense Request form")
	dev.call("PATCH", "/api/developer/workflows/"+defID, map[string]any{
		"name":          "Travel Expense Approval",
		"description":   "Reviews and approves travel expense requests before they post to the budget.",
		"trigger_event": "manual",
		"subject_type":  "form_record",
		"subject_config": map[string]string{
			"form_id": frm.formID,
		},
		"steps":          steps,
		"context_schema": []any{},
	})

	step("publishing workflow")
	dev.call("POST", "/api/developer/workflows/"+defID+"/publish", nil)

	step("creating automation rule: on form_submit, start this workflow")
	rule := dev.call("POST", "/api/automation/rules", map[string]any{
		"name":            "Auto-start approval on travel expense submission",
		"description":     "Automatic trigger — starts the moment a Travel Expense Request is submitted.",
		"trigger_type":    "form_submit",
		"workflow_name":   "Travel Expense Approval",
		"workflow_def_id": defID,
		"source_form_id":  frm.formID,
	})

	return workflowResult{defID: defID, automationRuleID: id(rule)}
}
