package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// TestThePlanCheckWarnsOfPageMistakes: a page no role opens, a grid on it
// whose metrics have no label and a rate in number format, and a guide
// rewritten whole go back with a warning each (2026-11-08).
func TestThePlanCheckWarnsOfPageMistakes(t *testing.T) {
	f := newPlanFixture(t, &multiScriptProvider{})
	sess := f.session(t)
	ws := f.q(`SELECT workspace_id::text FROM core.application WHERE id=$1::uuid`, f.appID)
	f.q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Sales') RETURNING id::text`, ws)
	board := f.q(`INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Start here') RETURNING id::text`, f.modelID, f.revID)
	guide := "# Guide\n\n## Who does what\n" + strings.Repeat("FP&A prepares the plan. ", 10) + "\n\n## Reading the numbers\n" + strings.Repeat("Amounts are EUR. ", 10)
	text := f.q(`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, content) VALUES ($1::uuid, 'text', $2) RETURNING id::text`, board, guide)

	var steps []aiassistant.ProposalStep
	add := func(tool string, params map[string]any) {
		b, _ := json.Marshal(params)
		steps = append(steps, aiassistant.ProposalStep{Tool: tool, Description: tool, Params: b})
	}
	add("create_metric", map[string]any{"name": "gross_margin", "formula": "amount / 2", "format": "number"})
	add("create_grid", map[string]any{"name": "P&L", "metrics": []string{"amount", "gross_margin"}, "dimensions": []string{"Cost Center"}})
	add("create_dashboard", map[string]any{"name": "Drivers"})
	add("add_dashboard_widget", map[string]any{"dashboard_id": ref(3), "widget_type": "grid", "ref_id": ref(2), "title": "P&L, EUR", "show_title": true})
	add("update_dashboard_widget", map[string]any{"widget_id": text, "content": "# Guide\n\nThe drivers live on [Drivers](dashboard:Drivers)."})
	add("reorder_dashboards", map[string]any{"dashboards": []string{"Start here", "Drivers"}})
	check, err := f.h.aiCheckProposal(f.ctx, sess.ID, f.modelID, f.revID, f.devID, steps)
	if err != nil || len(check.problems) > 0 {
		t.Fatalf("check: %v %v", err, check.problems)
	}
	all := strings.Join(check.warnings, "\n")
	for _, want := range []string{
		`step 3 creates the page "Drivers", which no role may open`,
		"step 4 puts a grid on a page whose metrics amount, gross_margin have no label",
		"step 4 shows the rates gross_margin as plain numbers",
		`step 5 (update_dashboard_widget) replaces the widget's whole text and drops the sections "Who does what", "Reading the numbers"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing warning %q in:\n%s", want, all)
		}
	}
}

// The reply holding a plan back for leave, and the request that names
// several things, are told from the ones that do not.
func TestLeaveToProposeAndManyItems(t *testing.T) {
	for reply, want := range map[string]bool{
		`Reply with: - "Yes" to accept the proposed owner-title mapping above, or give only the replacements`: true,
		`Say "proceed" to propose this plan as-is, or tell me which grids`:                                    true,
		"Once you confirm the titles I will propose the one plan that implements 1, 2 and 3.":                 true,
		"Which reading is right? 1) the count of entries 2) the total of the amounts":                         false,
		"I added the page and checked every row: 12 of 12 match.":                                             false,
	} {
		if got := asksLeaveToPropose(reply); got != want {
			t.Errorf("asksLeaveToPropose(%q) = %v, want %v", reply, got, want)
		}
	}
	long := "Part 4, the pages, one plan:\n1. Actuals & checks: put the Checks grid full width below the buttons and show all 13 check metrics.\n2. Add a page \"Assumptions & drivers\" with the input grids."
	if !requestNamesSeveral(long) || requestNamesSeveral("Rename the page to Drivers.") {
		t.Error("requestNamesSeveral tells a list from a single change wrongly")
	}
	if got := parseCoverage("Here: {\"missing\": [\"Cash_Flow grid\", \" \", \"Checks grid\"]} done"); strings.Join(got, "|") != "Cash_Flow grid|Checks grid" {
		t.Errorf("parseCoverage = %v", got)
	}
	if missing, extra := parseCoverageReply(`{"missing": [], "extra": ["step 4: adds a page Overview"]}`); len(missing) != 0 || len(extra) != 1 ||
		!strings.Contains(extraWarning(extra), "a direct order, and the plan also does what it did not ask: step 4: adds a page Overview") {
		t.Errorf("parseCoverageReply extra = %v %v", missing, extra)
	}
}
