package aiassistant

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// Turns before the last two keep their shape but not their bulk: a large
// tool result or plan is shortened, the current and previous turns stay whole.
func TestCompactHistoryShortensOlderTurnsOnly(t *testing.T) {
	big := strings.Repeat("x", 5000)
	plan, _ := json.Marshal(map[string]string{"steps": big})
	turn := func(n string) []ChatMessage {
		return []ChatMessage{
			{Role: "user", Content: "stage " + n},
			{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "c" + n, Name: "propose_actions", Arguments: plan}}},
			{Role: "tool", ToolCallID: "c" + n, ToolName: "propose_actions", Content: big},
		}
	}
	var msgs []ChatMessage
	for _, n := range []string{"1", "2", "3"} {
		msgs = append(msgs, turn(n)...)
	}
	got := CompactHistory(msgs)
	if len(got) != len(msgs) {
		t.Fatalf("messages = %d, want %d (shortened, never dropped)", len(got), len(msgs))
	}
	if !strings.Contains(got[2].Content, "an earlier result of 5000 characters") || len(got[2].Content) > 600 {
		t.Errorf("turn 1's tool result not shortened: %d chars", len(got[2].Content))
	}
	if args := string(got[1].ToolCalls[0].Arguments); !strings.Contains(args, "an earlier propose_actions call") || !json.Valid(got[1].ToolCalls[0].Arguments) {
		t.Errorf("turn 1's plan not shortened to valid JSON: %s", args)
	}
	for _, i := range []int{4, 5, 7, 8} {
		if len(got[i].Content)+len(got[i].ToolCalls) == 0 {
			continue
		}
		if got[i].Content != msgs[i].Content || (len(got[i].ToolCalls) > 0 && string(got[i].ToolCalls[0].Arguments) != string(plan)) {
			t.Errorf("message %d of the last two turns was changed", i)
		}
	}
	if msgs[2].Content != big {
		t.Error("the stored history was modified in place")
	}
}
