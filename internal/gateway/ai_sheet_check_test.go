package gateway

import (
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// The gate reads compare_with_sheet results by their call's id (a tool
// message stores no name), keeps each row's last comparison, and lets a
// sheet called done pass only when every compared row matches.
func TestSheetDoneGateReadsTheLastComparisons(t *testing.T) {
	call := func(id string) aiassistant.ChatMessage {
		return aiassistant.ChatMessage{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: id, Name: "compare_with_sheet"}}}
	}
	result := func(id, content string) aiassistant.ChatMessage {
		return aiassistant.ChatMessage{Role: "tool", ToolCallID: id, Content: content}
	}
	msgs := []aiassistant.ChatMessage{
		call("c1"),
		result("c1", "pnl_sales against sheet \"Actual_PnL\" row 6: 12 of 24 periods match. Different: 2026-01: sheet 5, model 4\n"+
			"pnl_cogs against sheet \"Actual_PnL\" row 7: 24 of 24 periods match\n2 of 1 rows match every period."),
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "x", Name: "list_metrics"}}},
		result("x", "net against sheet \"Actual_PnL\" row 9: 0 of 24 periods match"), // not a comparison
	}
	last := sheetComparisons(msgs)
	if got := last["actual_pnl"]; len(got) != 2 || got[6].matched != 12 || got[7].matched != 24 {
		t.Fatalf("comparisons = %+v", last)
	}
	if nudge := sheetDoneProblems([]string{"Actual_PnL"}, last); !strings.Contains(nudge, "row 6 (12 of 24 periods)") || strings.Contains(nudge, "row 7") {
		t.Errorf("nudge with row 6 differing:\n%s", nudge)
	}
	// Row 6 compared again, matching: the sheet passes.
	msgs = append(msgs, call("c2"), result("c2", "pnl_sales against sheet \"Actual_PnL\" row 6: 24 of 24 periods match"))
	if nudge := sheetDoneProblems([]string{"Actual_PnL"}, sheetComparisons(msgs)); nudge != "" {
		t.Errorf("every row matching, yet sent back:\n%s", nudge)
	}
	if nudge := sheetDoneProblems([]string{"Cash_Flow"}, sheetComparisons(msgs)); !strings.Contains(nudge, "Cash_Flow: no row of it compared yet") {
		t.Errorf("a sheet never compared:\n%s", nudge)
	}
}
