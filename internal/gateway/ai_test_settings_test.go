// Tests the "Test connection" tool-calling fix: aiTestSettings now sends a
// real Tools payload (the same AllTools() every live chat turn attaches)
// instead of a plain-text-only "reply with ok" probe, which used to pass
// for a model with no tool-calling support at all and only fail on the
// assistant's very first real turn. toolCallProbeResult is the pure
// decision function extracted from the handler so both outcomes — a model
// that calls a tool vs. one that ignores the Tools payload and replies in
// prose — can be asserted directly without a live provider or network call.
package gateway

import (
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

func TestToolCallProbeResult_ToolCallingModelSucceeds(t *testing.T) {
	resp := providers.ChatResponse{
		FinishReason: "tool_calls",
		Message: providers.Message{
			ToolCalls: []providers.ToolCall{
				{ID: "call_1", Name: "get_model_summary", Arguments: []byte(`{}`)},
			},
		},
	}
	ok, msg := toolCallProbeResult(resp)
	if !ok {
		t.Fatalf("expected ok=true for a tool_calls finish reason, got ok=false msg=%q", msg)
	}
	if msg == "" {
		t.Error("expected a non-empty confirmation message")
	}
}

func TestToolCallProbeResult_PlainTextModelFails(t *testing.T) {
	// A model that ignores the Tools payload entirely and just replies in
	// prose — the exact failure mode a plain-text-only probe could never
	// catch, since "reply with ok" always succeeds regardless.
	resp := providers.ChatResponse{
		FinishReason: "stop",
		Message: providers.Message{
			Content: "ok",
		},
	}
	ok, msg := toolCallProbeResult(resp)
	if ok {
		t.Fatal("expected ok=false for a plain-text reply with no tool call")
	}
	if msg == "" {
		t.Error("expected a non-empty error message explaining why")
	}
}

func TestToolCallProbeResult_ToolCallsFinishReasonButNoCallsIsStillAFailure(t *testing.T) {
	// Defensive case: some providers could in principle report the
	// tool_calls finish reason without actually attaching any tool calls.
	resp := providers.ChatResponse{
		FinishReason: "tool_calls",
		Message:      providers.Message{ToolCalls: nil},
	}
	ok, _ := toolCallProbeResult(resp)
	if ok {
		t.Fatal("expected ok=false when FinishReason is tool_calls but ToolCalls is empty")
	}
}
