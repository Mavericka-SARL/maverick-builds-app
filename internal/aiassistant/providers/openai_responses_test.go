package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A model chat completions refuses with reasoning and tools moves to the
// Responses API: text and a tool call stream back, usage is read, and the
// turn's reasoning goes back with the tool call on the next call.
func TestOpenAIResponsesFallback(t *testing.T) {
	var second map[string]any
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Function tools with reasoning_effort are not supported for gpt-x in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.","type":"invalid_request_error"}}`)
		case "/responses":
			calls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["reasoning"].(map[string]any)["effort"] != "high" {
				t.Errorf("effort not sent: %v", body["reasoning"])
			}
			if calls == 2 {
				second = body
			}
			w.Header().Set("Content-Type", "text/event-stream")
			ev := func(v string) { _, _ = fmt.Fprintf(w, "event: x\ndata: %s\n\n", v) }
			ev(`{"type":"response.output_text.delta","delta":"Reading "}`)
			ev(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"list_metrics","arguments":""}}`)
			ev(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"search\":"}`)
			ev(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"rev\"}"}`)
			ev(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"ENC","summary":[]},{"type":"message","content":[{"type":"output_text","text":"Reading "}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"list_metrics","arguments":"{\"search\":\"rev\"}"}],"usage":{"input_tokens":1000,"output_tokens":50,"input_tokens_details":{"cached_tokens":800}}}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	p := NewOpenAIAt("k", srv.URL, srv.Client()).WithReasoningEffort("high")
	req := ChatRequest{Model: "gpt-x", SystemPrompt: "sys", Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []ToolDef{{Name: "list_metrics", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}}
	var text strings.Builder
	var progress []string
	req.OnToolArgs = func(name string, args []byte) { progress = append(progress, name+":"+string(args)) }
	resp, err := p.ChatStream(context.Background(), req, func(d string) { text.WriteString(d) })
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "Reading " || resp.Message.Content != "Reading " || resp.FinishReason != "tool_calls" {
		t.Fatalf("text %q / %q, finish %s", text.String(), resp.Message.Content, resp.FinishReason)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].ID != "call_1" || string(resp.Message.ToolCalls[0].Arguments) != `{"search":"rev"}` {
		t.Fatalf("tool calls %+v", resp.Message.ToolCalls)
	}
	if len(progress) != 2 || progress[1] != `list_metrics:{"search":"rev"}` {
		t.Errorf("tool args progress %v", progress)
	}
	if resp.Usage.InputTokens != 200 || resp.Usage.CacheReadTokens != 800 || resp.Usage.OutputTokens != 50 {
		t.Errorf("usage %+v", resp.Usage)
	}
	// The next call of the turn: straight to /responses, reasoning before the call, then its output.
	req.Messages = append(req.Messages, resp.Message, Message{Role: "tool", ToolCallID: "call_1", Content: "Metrics: revenue"})
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "gpt-x", Messages: req.Messages, Tools: req.Tools}); err == nil {
		t.Log("non-streamed call parsed an event stream as JSON — expected an error here is fine")
	}
	if calls != 2 || second == nil {
		t.Fatalf("second call did not go to /responses (calls %d)", calls)
	}
	items, _ := second["input"].([]any)
	var kinds []string
	for _, it := range items {
		m := it.(map[string]any)
		k, _ := m["type"].(string)
		if k == "" {
			k = m["role"].(string)
		}
		switch k {
		case "reasoning":
			if m["encrypted_content"] != "ENC" {
				t.Errorf("reasoning item %v", m)
			}
		case "function_call":
			if m["call_id"] != "call_1" || m["name"] != "list_metrics" {
				t.Errorf("function_call %v", m)
			}
		case "function_call_output":
			if m["call_id"] != "call_1" || m["output"] != "Metrics: revenue" {
				t.Errorf("output %v", m)
			}
		}
		kinds = append(kinds, k)
	}
	if got := strings.Join(kinds, ","); got != "user,reasoning,assistant,function_call,function_call_output" {
		t.Errorf("second input order %s", got)
	}
}

// A stream's error event keeps its message, whether it stands on the event
// or in its error object (2026-11-12: a key out of credits read only
// "openai: responses: ").
func TestOpenAIResponsesStreamErrorMessage(t *testing.T) {
	for _, ev := range []string{
		`{"type":"error","error":{"message":"You have no credits remaining.","code":"insufficient_quota"}}`,
		`{"type":"error","message":"You have no credits remaining.","code":"insufficient_quota"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", ev)
		}))
		p := NewOpenAIAt("k", srv.URL, srv.Client()).WithReasoningEffort("low")
		responsesModels.Store("m-credits", true)
		_, err := p.ChatStream(context.Background(), ChatRequest{Model: "m-credits", Messages: []Message{{Role: "user", Content: "hi"}}}, func(string) {})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "You have no credits remaining") || !strings.Contains(err.Error(), "insufficient_quota") {
			t.Errorf("event %s: error %v", ev, err)
		}
	}
}
