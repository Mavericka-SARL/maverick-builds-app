package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// A plan that builds pages while input metrics hold nothing and nothing fills
// them is warned (2026-11-06: a build had every input grid at 0 values while
// its sheets were "done"); a plan that only creates inputs is not.
func TestPlanWarnsOfEmptyInputs(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	d.metric["headcount"] = idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{
		"name": "headcount", "is_input": true, "revision_id": d.revID, "agg_rule": "sum", "format": "number",
	}))
	d.call("POST", "/api/developer/grids/"+d.gridID+"/metrics/"+d.metric["headcount"], d.dev, nil)
	step := func(tool string) aiassistant.ProposalStep {
		raw, _ := json.Marshal(map[string]any{"name": "x", "is_input": true})
		return aiassistant.ProposalStep{Tool: tool, Params: raw}
	}
	ctx := context.Background()
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if got := emptyInputWarnings(ctx, tx, d.modelID, d.revID, []aiassistant.ProposalStep{step("create_metric")}); len(got) != 0 {
		t.Errorf("a plan creating inputs, building no page, was warned: %q", got)
	}
	got := emptyInputWarnings(ctx, tx, d.modelID, d.revID, []aiassistant.ProposalStep{step("add_dashboard_widget")})
	if len(got) != 1 || !strings.Contains(got[0], "(headcount)") || !strings.Contains(got[0], "compare_with_sheet") {
		t.Errorf("warnings %q, want one naming headcount alone (the plan's other inputs hold values)", got)
	}
}
