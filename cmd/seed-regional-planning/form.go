package main

type formResult struct {
	formID    string
	mappingID string
}

// buildForm creates "Travel Expense Request" and maps its "amount" field
// into the travel_cost grid metric (POST /api/developer/form-integrations),
// with dimension_mappings tying the form's department/period fields to the
// Department Budget grid's dimensions — so an approved submission posts a
// real value into runtime.fact_input at the right cell.
func buildForm(dev *api, d dims, m metrics) formResult {
	step("creating form \"Travel Expense Request\"")
	form := dev.call("POST", "/api/forms", map[string]any{
		"name":  "travel_expense_request",
		"label": "Travel Expense Request",
		"fields": []map[string]any{
			{"name": "department", "label": "Department", "type": "dimension", "required": true, "dimension_id": d.departmentID},
			{"name": "period", "label": "Period", "type": "dimension", "required": true, "dimension_id": d.periodID},
			{"name": "amount", "label": "Amount", "type": "number", "required": true},
			{"name": "justification", "label": "Justification", "type": "text", "required": false},
		},
	})
	formID := id(form)

	step("mapping form field \"amount\" -> travel_cost metric (live, posts on approval)")
	mapping := dev.call("POST", "/api/developer/form-integrations", map[string]any{
		"form_id":          formID,
		"grid_id":          "",
		"name":             "Post approved travel expenses",
		"source_field":     "amount",
		"target_metric_id": m.travelCost,
		"aggregation":      "sum",
		"posting_statuses": []string{"approved"},
		"dimension_mappings": map[string]string{
			d.departmentID: "department",
			d.periodID:     "period",
		},
		"live_posting": true,
	})

	return formResult{formID: formID, mappingID: id(mapping)}
}
