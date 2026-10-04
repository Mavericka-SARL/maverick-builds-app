package aiassistant_test

import (
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// A session stored before every sibling tool call was answered (two
// propose_actions in one message, only the first answered) must still replay:
// providers refuse a history with an unanswered tool call, so without a
// stand-in every later turn of that session fails.
func TestMessagesToProviderHistory_AnswersUnansweredToolCalls(t *testing.T) {
	history := aiassistant.MessagesToProviderHistory([]aiassistant.ChatMessage{
		{Role: "user", Content: "build it"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "a", Name: "propose_actions"}, {ID: "b", Name: "propose_actions"}}},
		{Role: "tool", ToolCallID: "a", Content: "Proposal created"},
		{Role: "assistant", Content: "Executed."},
		{Role: "user", Content: "continue"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "c", Name: "list_metrics"}}},
	})

	answered := map[string]int{}
	var calls []string
	for i, m := range history {
		for _, tc := range m.ToolCalls {
			calls = append(calls, tc.ID)
		}
		if m.Role == "tool" {
			answered[m.ToolCallID] = i
		}
	}
	for _, id := range calls {
		if _, ok := answered[id]; !ok {
			t.Errorf("tool call %q has no tool message in the replayed history", id)
		}
	}
	// The stand-in for "b" sits inside its own group, before the next assistant turn.
	if answered["b"] != 3 || history[4].Role != "assistant" {
		t.Errorf("stand-in for b at index %d, want 3 (right after a's result); history: %+v", answered["b"], history)
	}
	if len(history) != 8 {
		t.Errorf("history has %d messages, want 8 (6 stored + 2 stand-ins)", len(history))
	}
}
