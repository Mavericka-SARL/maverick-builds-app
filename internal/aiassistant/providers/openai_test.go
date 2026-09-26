package providers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestOpenAIProvider_Chat_MapsRequestAndResponse(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1,
			"model": "gpt-4", "choices": [{
				"index": 0, "finish_reason": "stop",
				"message": {"role": "assistant", "content": "Hello there!"}
			}]
		}`))
	}))
	defer server.Close()

	p := NewOpenAICompatible("test-key", server.URL, "openai")
	resp, err := p.Chat(t.Context(), ChatRequest{
		Model:        "gpt-4",
		SystemPrompt: "You are a helpful assistant.",
		Messages:     []Message{{Role: "user", Content: "hi"}},
		Tools: []ToolDef{{
			Name: "create_metric", Description: "Create a metric",
			Parameters: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
		}},
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
	if len(msgs) != 2 {
		t.Fatalf("expected 2 request messages (system + user), got %d: %v", len(msgs), msgs)
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "You are a helpful assistant." {
		t.Fatalf("expected system message first, got %v", first)
	}
	second := msgs[1].(map[string]any)
	if second["role"] != "user" || second["content"] != "hi" {
		t.Fatalf("expected user message second, got %v", second)
	}

	tools, _ := captured["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool in request, got %d", len(tools))
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "create_metric" {
		t.Fatalf("unexpected tool name: %v", fn["name"])
	}
}

func TestOpenAIProvider_Chat_ToolCallRoundtrip(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-2", "object": "chat.completion", "created": 1,
			"model": "gpt-4", "choices": [{
				"index": 0, "finish_reason": "tool_calls",
				"message": {
					"role": "assistant", "content": "",
					"tool_calls": [{"id": "call_1", "type": "function",
						"function": {"name": "create_metric", "arguments": "{\"name\":\"revenue\"}"}}]
				}
			}]
		}`))
	}))
	defer server.Close()

	p := NewOpenAICompatible("test-key", server.URL, "openai")
	resp, err := p.Chat(t.Context(), ChatRequest{
		Model: "gpt-4",
		Messages: []Message{
			{Role: "user", Content: "create a revenue metric"},
			{Role: "assistant", ToolCalls: []ToolCall{
				{ID: "call_1", Name: "create_metric", Arguments: json.RawMessage(`{"name":"revenue"}`)},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "Metric 'revenue' created (id: abc)"},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("unexpected finish reason: %q", resp.FinishReason)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call in response, got %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "create_metric" || string(tc.Arguments) != `{"name":"revenue"}` {
		t.Fatalf("unexpected tool call: %+v", tc)
	}

	msgs, _ := captured["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 request messages, got %d: %v", len(msgs), msgs)
	}
	assistantMsg := msgs[1].(map[string]any)
	if assistantMsg["role"] != "assistant" {
		t.Fatalf("expected assistant message at index 1, got %v", assistantMsg)
	}
	toolCalls, _ := assistantMsg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected assistant message to carry 1 tool_call, got %d", len(toolCalls))
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" {
		t.Fatalf("expected tool-result message with matching tool_call_id, got %v", toolMsg)
	}
}

func TestOpenAIProvider_Chat_EmptyChoicesErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": "chatcmpl-3", "object": "chat.completion", "created": 1, "model": "gpt-4", "choices": []}`))
	}))
	defer server.Close()

	p := NewOpenAICompatible("test-key", server.URL, "openai")
	_, err := p.Chat(t.Context(), ChatRequest{Model: "gpt-4", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected an error for an empty choices array")
	}
	if !strings.Contains(err.Error(), "openai") {
		t.Fatalf("expected error to be labeled with provider name, got: %v", err)
	}
}

func TestOpenAIProvider_Chat_HTTPErrorWrappedWithLabel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": {"message": "boom", "type": "server_error"}}`))
	}))
	defer server.Close()

	// Mistral and DeepSeek both reuse OpenAIProvider via NewOpenAICompatible —
	// the label must propagate into the wrapped error so multi-provider
	// failures are distinguishable in logs.
	p := NewOpenAICompatible("test-key", server.URL, "mistral")
	_, err := p.Chat(t.Context(), ChatRequest{Model: "mistral-large", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if !strings.HasPrefix(err.Error(), "mistral:") {
		t.Fatalf("expected error prefixed with provider label 'mistral:', got: %v", err)
	}
}

func writeSSE(w http.ResponseWriter, chunks []string) {
	f := w.(http.Flusher)
	for _, c := range chunks {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
		f.Flush()
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	f.Flush()
}

func TestOpenAIProvider_ChatStream_DeliversDeltasInOrderAndFinalContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{"content":" there!"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		})
	}))
	defer server.Close()

	p := NewOpenAICompatible("test-key", server.URL, "openai")
	var deltas []string
	resp, err := p.ChatStream(t.Context(), ChatRequest{
		Model:    "gpt-4",
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

// TestOpenAIProvider_ChatStream_ReassemblesToolCallFragments is a regression
// test for the index-keyed accumulation logic: OpenAI streams tool-call
// arguments as string fragments tagged by index, with id/name only present
// on the first fragment for that index — get the accumulation wrong and
// tool calls silently arrive with truncated or malformed JSON arguments.
func TestOpenAIProvider_ChatStream_ReassemblesToolCallFragments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"create_metric","arguments":""}}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"name\":"}}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"revenue\"}"}}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		})
	}))
	defer server.Close()

	p := NewOpenAICompatible("test-key", server.URL, "openai")
	resp, err := p.ChatStream(t.Context(), ChatRequest{
		Model:    "gpt-4",
		Messages: []Message{{Role: "user", Content: "create a revenue metric"}},
	}, func(string) {})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("unexpected finish reason: %q", resp.FinishReason)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 reassembled tool call, got %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "create_metric" {
		t.Fatalf("unexpected tool call identity: %+v", tc)
	}
	if string(tc.Arguments) != `{"name":"revenue"}` {
		t.Fatalf("expected reassembled arguments %q, got %q", `{"name":"revenue"}`, tc.Arguments)
	}
}

func TestOpenAIProvider_ChatStream_ErrorWrappedWithLabel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": {"message": "boom", "type": "server_error"}}`))
	}))
	defer server.Close()

	p := NewOpenAICompatible("test-key", server.URL, "mistral")
	_, err := p.ChatStream(t.Context(), ChatRequest{
		Model:    "mistral-large",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(string) {})
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if !strings.HasPrefix(err.Error(), "mistral:") {
		t.Fatalf("expected error prefixed with provider label 'mistral:', got: %v", err)
	}
}

// Gemini's OpenAI-compatible API sits under a path prefix
// (…/v1beta/openai), unlike Mistral and DeepSeek. The request must keep the
// prefix, carry the key as a bearer token, and a tool call must come back.
func TestOpenAICompatible_BaseURLWithPathPrefix(t *testing.T) {
	var path, auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "c1", "object": "chat.completion", "created": 1, "model": "gemini-3.8-flash",
			"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {"role": "assistant", "content": "",
				"tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "list_models", "arguments": "{}"}}]}}]
		}`))
	}))
	defer server.Close()

	p := NewOpenAICompatible("gemini-key", server.URL+"/v1beta/openai", "google")
	resp, err := p.Chat(t.Context(), ChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []Message{{Role: "user", Content: "list my models"}},
		Tools:    []ToolDef{{Name: "list_models", Description: "List models", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if path != "/v1beta/openai/chat/completions" {
		t.Fatalf("request path = %q, want /v1beta/openai/chat/completions", path)
	}
	if auth != "Bearer gemini-key" {
		t.Fatalf("Authorization = %q", auth)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "list_models" {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
}
