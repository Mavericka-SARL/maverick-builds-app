package main

import (
	"fmt"
	"time"
)

// runFormAggregationTests exercises all 7 form->metric aggregation modes
// (sum/average/min/max/count/last/replace) by posting the same 3 records
// (amount 10, 20, 30, all approved) through 7 different mappings, each
// targeting its own scalar input metric. "sum" and "replace" additionally
// get a manual direct-entry value written first, to test the documented
// (and previously buggy — see model_transfer.go's export/import fix this
// session) direct-vs-form-posted interaction: sum should be ADDITIVE with
// the direct value, replace should EXCLUDE it entirely.
func runFormAggregationTests(dev *api) {
	modes := []string{"sum", "average", "min", "max", "count", "last", "replace"}
	metricIDs := map[string]string{}
	for _, mode := range modes {
		metricIDs[mode] = createInputMetric(dev, "agg_"+mode+"_metric")
	}

	step("writing manual direct-entry values before any form posting (sum=100, replace=100)")
	writeCell(dev, metricIDs["sum"], nil, 100)
	writeCell(dev, metricIDs["replace"], nil, 100)

	step("creating form \"QA Posting Form\" (field: amount)")
	form := dev.call("POST", "/api/forms", map[string]any{
		"name":  "qa_posting_form",
		"label": "QA Posting Form",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "type": "number", "required": true},
		},
	})
	formID := id(form)

	step("mapping \"amount\" -> each of the 7 aggregation-mode metrics")
	for _, mode := range modes {
		dev.call("POST", "/api/developer/form-integrations", map[string]any{
			"form_id":            formID,
			"grid_id":            "",
			"name":               "QA " + mode + " mapping",
			"source_field":       "amount",
			"target_metric_id":   metricIDs[mode],
			"aggregation":        mode,
			"posting_statuses":   []string{"approved"},
			"dimension_mappings": map[string]string{},
			"live_posting":       true,
		})
	}

	step("posting 3 records (amount 10, 20, 30), each created draft then transitioned to approved")
	for _, amount := range []float64{10, 20, 30} {
		rec := dev.call("POST", fmt.Sprintf("/api/forms/%s/records", formID), map[string]any{
			"data": map[string]any{"amount": amount},
		})
		recordID := id(rec)
		dev.call("PUT", "/api/records/"+recordID, map[string]any{
			"data":   map[string]any{"amount": amount},
			"status": "approved",
		})
	}
	// applyFormMappings runs in a background goroutine per record/transition
	// (see handler.go's recordAction) — give it a moment to settle before
	// reading the grid back. This is a correctness test, not a latency
	// test, so a generous fixed wait is fine here.
	time.Sleep(1500 * time.Millisecond)

	grid := dev.call("GET", fmt.Sprintf("/api/grid?revision_id=%s", dev.revisionID), nil)
	totals, _ := grid["totals"].(map[string]any)

	check := func(name, mode string, expected float64) {
		v, ok := totals[metricIDs[mode]]
		if !ok {
			record(name, false, "missing from totals")
			return
		}
		f, _ := v.(float64)
		record(name, f == expected, fmt.Sprintf("expected %v got %v", expected, v))
	}
	check("sum: direct(100) + posted(10+20+30) = 160 (additive)", "sum", 160)
	check("average: posted average(10,20,30) = 20", "average", 20)
	check("min: posted min(10,20,30) = 10", "min", 10)
	check("max: posted max(10,20,30) = 30", "max", 30)
	check("count: 3 posted records", "count", 3)
	check("last: most recent posted value = 30", "last", 30)
	check("replace: posted(30) REPLACES direct(100), not additive", "replace", 30)
}
