package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// A carried-forward pair written as gpt-5-mini wrote the Audience sheet
// (2026-11-08): placeholders for every id, the formulas in reading order,
// the metrics placed on their monthly grid last — and one placeholder
// miscounted, pointing at the add step itself. It reaches the developer as a
// plan that runs: placeholders become the names they stand for, the cycle's
// metrics are created with 0, and their formulas are set once every metric is
// on the time grid.
func TestSendMessage_CarriedForwardPairWithPlaceholdersRuns(t *testing.T) {
	plan := []map[string]any{
		{"tool": "create_dimension", "description": "Months", "params": map[string]any{"name": "period", "dimension_type": "time",
			"time_granularity": "month", "fiscal_year_start_month": 1, "members": []map[string]any{{"code": "FY27", "label": "FY27"}}}},
		{"tool": "generate_time_members", "description": "Months", "params": map[string]any{"dimension_id": "<created in step 1>", "start": "2027-01-01", "end": "2027-03-31", "parent_code": "FY27"}},
		{"tool": "create_grid", "description": "Audience grid", "params": map[string]any{"name": "Audience", "dimensions": []string{"<created in step 1>"}}},
		turnMetricStep("new_followers", map[string]any{"is_input": true}),
		turnMetricStep("opening_followers", map[string]any{"formula": "LAG(closing_followers, 1, 0)", "time_summary": "first"}),
		turnMetricStep("closing_followers", map[string]any{"formula": "opening_followers + new_followers", "time_summary": "last"}),
		{"tool": "add_grid_metric", "description": "Add 'new_followers' to Audience", "params": map[string]any{"grid_id": "<created in step 3>", "metric_id": "<created in step 4>"}},
		{"tool": "add_grid_metric", "description": "Add 'opening_followers' to Audience", "params": map[string]any{"grid_id": "<created in step 3>", "metric_id": "<created in step 5>"}},
		{"tool": "add_grid_metric", "description": "Add 'closing_followers' to Audience", "params": map[string]any{"grid_id": "<created in step 3>", "metric_id": "<created in step 9>"}},
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{proposeResp("c1", plan...)}}
	env := newAITurnEnv(t, fake)
	status, sse := env.send("build the audience roll-forward")
	if status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s\ntool results: %q", status, sse, toolResults(t, env))
	}
	if results := toolResults(t, env); len(results) != 1 || !strings.HasPrefix(results[0], "Proposal created") {
		t.Fatalf("the plan went back: %q", results)
	}
	ps, _ := env.proposals.ListProposals(context.Background(), env.session)
	var order []string
	lastPlace, firstSet := -1, -1
	for i, st := range ps[0].Steps {
		order = append(order, st.Tool+" "+string(st.Params))
		if st.Tool == "add_grid_metric" {
			lastPlace = i
		}
		if st.Tool == "update_metric" && firstSet < 0 {
			firstSet = i
		}
	}
	if firstSet < 0 || firstSet < lastPlace {
		t.Errorf("the formulas are not set after the metrics are placed:\n%s", strings.Join(order, "\n"))
	}
	if all := strings.Join(order, "\n"); strings.Contains(all, "created in step") || !strings.Contains(all, `"metric_id": "closing_followers"`) {
		t.Errorf("placeholders left, or the miscounted one not read as closing_followers:\n%s", all)
	}
}

// A plan placing a dimension with no members on a grid is told to add the
// members first; a dimension with members is not mentioned.
func TestSendMessage_WarnsOfADimensionWithoutMembersOnAGrid(t *testing.T) {
	plan := []map[string]any{
		{"tool": "create_dimension", "description": "Regions", "params": map[string]any{"name": "sales_region"}},
		{"tool": "create_dimension", "description": "Channels", "params": map[string]any{"name": "sales_channel",
			"members": []map[string]any{{"code": "WEB", "label": "Web"}}}},
		turnMetricStep("sales_amount", map[string]any{"is_input": true}),
		{"tool": "create_grid", "description": "Sales grid", "params": map[string]any{"name": "Sales",
			"dimensions": []string{"sales_region", "sales_channel"}, "metrics": []string{"sales_amount"}}},
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{proposeResp("c1", plan...), proposeResp("c2", plan...)}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build the sales grid"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s\n%q", status, sse, toolResults(t, env))
	}
	results := toolResults(t, env)
	if !strings.Contains(results[0], "dimension sales_region has no members as this plan leaves it — add its members first") {
		t.Errorf("no warning for sales_region:\n%s", results[0])
	}
	if strings.Contains(results[0], "dimension sales_channel has no members") {
		t.Errorf("a dimension with members was warned of:\n%s", results[0])
	}
}
