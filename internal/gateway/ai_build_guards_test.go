package gateway

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// The four replies the Creator & Agency build ended turns with, proposing
// nothing, are announcements of a proposal.
func TestClaimsUnproposedChangeHearsSendSubmitPost(t *testing.T) {
	for _, reply := range []string{
		"I'll send the corrected single proposal now.",
		"…so the plan check accepts it.\n\nI will send that corrected single proposal now.",
		"I'll fix the plan by ensuring every create_metric precedes any step that uses it. I'll send the corrected single proposal now.",
		"Let me now submit the full plan for the Audience sheet.",
		"I am going to post the plan with all 44 steps.",
	} {
		if !claimsUnproposedChange(reply) {
			t.Errorf("not heard as an announced proposal: %q", reply)
		}
	}
	for _, reply := range []string{
		"The Audience sheet has 44 rows; 41 match. Rows 20-22 differ because the churn rate is missing.",
		"Which reading do you mean: the plan or the forecast?",
	} {
		if claimsUnproposedChange(reply) {
			t.Errorf("heard as an announced proposal: %q", reply)
		}
	}
}

// A plan failing the same way twice goes to "the steps that pass", then to
// stop — renumbered steps and fresh ids read as the same failure.
func TestPlanRetriesEscalateOnARepeatedFailure(t *testing.T) {
	var r planRetries
	p1 := []string{`step 36 (add_grid_metric — Add 'gl_rows'): metric "<created in step 33>" not found in the working revision`}
	p2 := []string{`step 37 (add_grid_metric — Add 'gl_rows'): metric "<created in step 34>" not found in the working revision`}
	if r.repeats(p1) {
		t.Fatal("first failure read as a repeat")
	}
	if !r.repeats(p2) {
		t.Fatal("the same failure renumbered not read as a repeat")
	}
	if m := r.escalate(); m != retryPartial {
		t.Errorf("first escalation = %v, want retryPartial", m)
	}
	if m := r.escalate(); m != retryStop {
		t.Errorf("second escalation = %v, want retryStop", m)
	}
	if r.repeats([]string{"step 2 (create_metric — x): formula does not parse"}) {
		t.Error("a different failure read as a repeat")
	}
}

// Warnings count one by one: a plan whose warnings were all given before —
// alone or with others, beside failures or not — goes to the developer.
func TestWarnedBeforeCountsEachWarning(t *testing.T) {
	var r planRetries
	a := "step 3 (create_metric x): a currency amount without format_currency"
	b := "closing_cash looks like a level"
	if r.warnedBefore([]string{a, b}) {
		t.Fatal("new warnings read as given")
	}
	if !r.warnedBefore([]string{strings.Replace(a, "step 3", "step 5", 1)}) {
		t.Error("a warning given before, renumbered, read as new")
	}
	if r.warnedBefore([]string{b, "a third warning"}) {
		t.Error("a set with a new warning read as given")
	}
}

func TestUnreadablePlanResultSaysWhyAndWhatNext(t *testing.T) {
	cut := unreadablePlanResult(48211, true, nil)
	bad := unreadablePlanResult(900, false, errors.New("unexpected end of JSON input"))
	empty := unreadablePlanResult(12, false, nil)
	for _, s := range []string{cut, bad, empty} {
		if !strings.HasPrefix(s, "Proposal NOT created") || !strings.Contains(s, "in parts") {
			t.Errorf("result does not say to propose in parts: %q", s)
		}
	}
	if !strings.Contains(cut, "cut off") || !strings.Contains(cut, "48211") {
		t.Errorf("cut-off result: %q", cut)
	}
	if !strings.Contains(bad, "unexpected end of JSON input") || !strings.Contains(empty, "no steps") {
		t.Errorf("results: %q / %q", bad, empty)
	}
}

// The session's comparisons are rebuilt from its compare_with_sheet calls:
// single rows and "rows" lists, the call's first_column and members where a
// row gives none, the last call for a row winning.
func TestSessionCompareRows(t *testing.T) {
	call := func(args map[string]any) aiassistant.ChatMessage {
		b, _ := json.Marshal(args)
		return aiassistant.ChatMessage{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "c", Name: "compare_with_sheet", Arguments: b}}}
	}
	history := []aiassistant.ChatMessage{
		call(map[string]any{"file": "w.xlsx", "sheet": "P&L", "first_column": "C", "members": map[string]string{"Scenario": "BASE"},
			"rows": []map[string]any{{"row": 6, "metric": "revenue"}, {"row": 7, "metric": "cogs", "first_column": "D"}}}),
		call(map[string]any{"file": "w.xlsx", "sheet": "Cash", "row": 9, "first_column": "C", "metric": "cash", "members": map[string]string{"Scenario": "BASE"}}),
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "x", Name: "list_metrics", Arguments: json.RawMessage(`{}`)}}},
		call(map[string]any{"file": "w.xlsx", "sheet": "P&L", "row": 6, "first_column": "C", "metric": "net_revenue", "members": map[string]string{"Scenario": "BASE"}}),
	}
	got := sessionCompareRows(history)
	if len(got) != 3 {
		t.Fatalf("rows = %+v, want 3", got)
	}
	if got[0].Metric != "net_revenue" || got[0].Row != 6 || got[0].Members["Scenario"] != "BASE" {
		t.Errorf("row 6 = %+v: the last call should win", got[0])
	}
	if got[1].Metric != "cogs" || got[1].FirstColumn != "D" || got[1].Members["Scenario"] != "BASE" {
		t.Errorf("row 7 = %+v", got[1])
	}
	if got[2].Sheet != "Cash" || got[2].Metric != "cash" {
		t.Errorf("cash row = %+v", got[2])
	}
	if groups := groupSpecs(got); len(groups) != 2 || len(groups[0]) != 2 {
		t.Errorf("groups = %+v", groups)
	}
}

// A single-row result's differing periods join its line; a never-comparable
// row is left out of the check by its caller.
func TestJoinDiffLines(t *testing.T) {
	res := "ending_clients against sheet \"Agency Capacity\" row 9: 0 of 24 periods match. Different:\n" +
		"- 2026-01: sheet 10, model 12\n- 2026-02: sheet 11, model 12\n- 2026-03: sheet 12, model 13\n- 2026-04: sheet 13, model 14\n- 2026-05: sheet 14, model 15"
	got := joinDiffLines(res)
	if len(got) != 1 {
		t.Fatalf("lines = %q", got)
	}
	want := "ending_clients against sheet \"Agency Capacity\" row 9: 0 of 24 periods match. Different: 2026-01: sheet 10, model 12; 2026-02: sheet 11, model 12; 2026-03: sheet 12, model 13; and 2 more"
	if got[0] != want {
		t.Errorf("line = %q\nwant  %q", got[0], want)
	}
	multi := "a against sheet \"S\" row 6: 24 of 24 periods match\nb against sheet \"S\" row 7: 1 of 24 periods match. Different: 2026-02: sheet 1, model 2; and 22 more\n2 of 2 rows match every period."
	if got := joinDiffLines(multi); len(got) != 3 || !compareLine.MatchString(got[1]) {
		t.Errorf("multi-row lines = %q", got)
	}
}

func TestTheRequestNamesTheInput(t *testing.T) {
	low := strings.ToLower("Set the Base monthly paid marketing assumption to 6,000; the workbook's 5,300 is out of date.")
	if !namesMetric(low, "monthly_paid_marketing", "") || !namesMetric(low, "mkt", "Monthly paid marketing (EUR)") {
		t.Error("the input named by its words or its label is not heard")
	}
	if namesMetric(low, "monthly_general_admin_travel", "General admin and travel (EUR)") {
		t.Error("an input the request does not name is heard")
	}
}
