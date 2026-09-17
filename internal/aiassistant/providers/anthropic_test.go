package providers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func newTestAnthropicProvider(baseURL string) *AnthropicProvider {
	return &AnthropicProvider{client: anthropic.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(baseURL))}
}

func TestAnthropicProvider_Chat_TextResponse(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-x",
			"content": [{"type": "text", "text": "Hello there!"}],
			"stop_reason": "end_turn", "stop_sequence": null,
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`))
	}))
	defer server.Close()

	p := newTestAnthropicProvider(server.URL)
	resp, err := p.Chat(t.Context(), ChatRequest{
		Model:        "claude-x",
		SystemPrompt: "You are a helpful assistant.",
		Messages:     []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Content != "Hello there!" {
		t.Fatalf("unexpected content: %q", resp.Message.Content)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("unexpected finish reason: %q", resp.FinishReason)
	}

	msgs, _ := captured["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 request message, got %d: %v", len(msgs), msgs)
	}
	if captured["system"] == nil {
		t.Fatal("expected a system prompt in the request")
	}
}

func TestAnthropicProvider_Chat_ToolUseResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_2", "type": "message", "role": "assistant", "model": "claude-x",
			"content": [
				{"type": "text", "text": "Sure, creating it now."},
				{"type": "tool_use", "id": "toolu_1", "name": "create_metric", "input": {"name": "revenue"}}
			],
			"stop_reason": "tool_use", "stop_sequence": null,
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`))
	}))
	defer server.Close()

	p := newTestAnthropicProvider(server.URL)
	resp, err := p.Chat(t.Context(), ChatRequest{
		Model:    "claude-x",
		Messages: []Message{{Role: "user", Content: "create a revenue metric"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("expected stop_reason=tool_use to map to FinishReason=tool_calls, got %q", resp.FinishReason)
	}
	if resp.Message.Content != "Sure, creating it now." {
		t.Fatalf("unexpected content: %q", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Name != "create_metric" {
		t.Fatalf("unexpected tool call: %+v", tc)
	}
	var args map[string]string
	if err := json.Unmarshal(tc.Arguments, &args); err != nil || args["name"] != "revenue" {
		t.Fatalf("unexpected tool call arguments: %s (err=%v)", tc.Arguments, err)
	}
}

// TestAnthropicProvider_Chat_MergesConsecutiveToolResults is a regression
// test for the merge logic documented in anthropic.go: "Anthropic requires
// ALL tool results for one assistant turn in a single user message." Without
// it, two sequential tool-role messages would be sent as two separate user
// messages, which the real API rejects.
func TestAnthropicProvider_Chat_MergesConsecutiveToolResults(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_3", "type": "message", "role": "assistant", "model": "claude-x",
			"content": [{"type": "text", "text": "Both created."}],
			"stop_reason": "end_turn", "stop_sequence": null,
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`))
	}))
	defer server.Close()

	p := newTestAnthropicProvider(server.URL)
	_, err := p.Chat(t.Context(), ChatRequest{
		Model: "claude-x",
		Messages: []Message{
			{Role: "user", Content: "create revenue and headcount metrics"},
			{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "call_1", Name: "create_metric", Arguments: json.RawMessage(`{"name":"revenue"}`)},
				{ID: "call_2", Name: "create_metric", Arguments: json.RawMessage(`{"name":"headcount"}`)},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "Metric 'revenue' created"},
			{Role: "tool", ToolCallID: "call_2", Content: "Metric 'headcount' created"},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	msgs, _ := captured["messages"].([]any)
	// user, assistant, merged-user(tool results) = 3, NOT 4.
	if len(msgs) != 3 {
		t.Fatalf("expected 3 request messages (tool results merged into one), got %d: %v", len(msgs), msgs)
	}
	assistantMsg := msgs[1].(map[string]any)
	if assistantMsg["role"] != "assistant" {
		t.Fatalf("expected assistant message at index 1, got %v", assistantMsg)
	}
	assistantBlocks, _ := assistantMsg["content"].([]any)
	if len(assistantBlocks) != 2 {
		t.Fatalf("expected assistant message to carry 2 tool_use blocks, got %d", len(assistantBlocks))
	}

	mergedMsg := msgs[2].(map[string]any)
	if mergedMsg["role"] != "user" {
		t.Fatalf("expected merged tool-result message to have role=user, got %v", mergedMsg["role"])
	}
	blocks, _ := mergedMsg["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 tool_result blocks merged into 1 message, got %d: %v", len(blocks), blocks)
	}
	for _, b := range blocks {
		block := b.(map[string]any)
		if block["type"] != "tool_result" {
			t.Fatalf("expected block type tool_result, got %v", block["type"])
		}
	}
	if blocks[0].(map[string]any)["tool_use_id"] != "call_1" || blocks[1].(map[string]any)["tool_use_id"] != "call_2" {
		t.Fatalf("expected tool_use_id order call_1, call_2, got %v", blocks)
	}
}

func TestAnthropicProvider_Chat_SkipsEmptyUserMessage(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_4", "type": "message", "role": "assistant", "model": "claude-x",
			"content": [{"type": "text", "text": "ok"}],
			"stop_reason": "end_turn", "stop_sequence": null,
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`))
	}))
	defer server.Close()

	p := newTestAnthropicProvider(server.URL)
	_, err := p.Chat(t.Context(), ChatRequest{
		Model: "claude-x",
		Messages: []Message{
			{Role: "user", Content: ""},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	msgs, _ := captured["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected the empty user message to be dropped, got %d messages: %v", len(msgs), msgs)
	}
}

func TestAnthropicProvider_Chat_ErrorWrappedWithLabel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	}))
	defer server.Close()

	p := newTestAnthropicProvider(server.URL)
	_, err := p.Chat(t.Context(), ChatRequest{
		Model:    "claude-x",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if !strings.HasPrefix(err.Error(), "anthropic:") {
		t.Fatalf("expected error prefixed with 'anthropic:', got: %v", err)
	}
}

// writeAnthropicSSE writes event/data pairs in the exact framing the SDK's
// ssestream decoder requires: "event: <type>\ndata: <json>\n\n" per event,
// dispatched on the blank line.
func writeAnthropicSSE(w http.ResponseWriter, events [][2]string) {
	f := w.(http.Flusher)
	for _, e := range events {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e[0], e[1])
		f.Flush()
	}
}

func TestAnthropicProvider_ChatStream_DeliversDeltasInOrderAndFinalContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeAnthropicSSE(w, [][2]string{
			{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there!"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`},
			{"message_stop", `{"type":"message_stop"}`},
		})
	}))
	defer server.Close()

	p := newTestAnthropicProvider(server.URL)
	var deltas []string
	resp, err := p.ChatStream(t.Context(), ChatRequest{
		Model:    "claude-x",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(delta string) { deltas = append(deltas, delta) })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if want := []string{"Hello", " there!"}; !slices.Equal(deltas, want) {
		t.Fatalf("expected deltas %v in order, got %v", want, deltas)
	}
	if resp.Message.Content != "Hello there!" {
		t.Fatalf("expected final content %q, got %q", "Hello there!", resp.Message.Content)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("unexpected finish reason: %q", resp.FinishReason)
	}
}

// TestAnthropicProvider_ChatStream_ReassemblesToolUseInput is a regression
// test for wiring Message.Accumulate correctly: tool input streams as
// input_json_delta string fragments (partial_json) that must be fully
// reassembled into valid JSON arguments by the time the stream ends.
func TestAnthropicProvider_ChatStream_ReassemblesToolUseInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeAnthropicSSE(w, [][2]string{
			{"message_start", `{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","content":[],"model":"claude-x","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"create_metric","input":{}}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"name\": \"reve"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"nue\"}"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":5}}`},
			{"message_stop", `{"type":"message_stop"}`},
		})
	}))
	defer server.Close()

	p := newTestAnthropicProvider(server.URL)
	resp, err := p.ChatStream(t.Context(), ChatRequest{
		Model:    "claude-x",
		Messages: []Message{{Role: "user", Content: "create a revenue metric"}},
	}, func(string) {})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("expected stop_reason=tool_use to map to FinishReason=tool_calls, got %q", resp.FinishReason)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 reassembled tool call, got %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Name != "create_metric" {
		t.Fatalf("unexpected tool call identity: %+v", tc)
	}
	var args map[string]string
	if err := json.Unmarshal(tc.Arguments, &args); err != nil || args["name"] != "revenue" {
		t.Fatalf("expected reassembled arguments {\"name\":\"revenue\"}, got %s (err=%v)", tc.Arguments, err)
	}
}

func TestAnthropicProvider_ChatStream_ErrorWrappedWithLabel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	}))
	defer server.Close()

	p := newTestAnthropicProvider(server.URL)
	_, err := p.ChatStream(t.Context(), ChatRequest{
		Model:    "claude-x",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(string) {})
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if !strings.HasPrefix(err.Error(), "anthropic:") {
		t.Fatalf("expected error prefixed with 'anthropic:', got: %v", err)
	}
}
