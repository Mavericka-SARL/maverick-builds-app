package providers

import (
	"context"
	"encoding/json"
)

// ToolDef is the JSON schema declaration for a single tool the LLM may call.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema object
}

// ToolCall is one tool invocation requested by the assistant.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Message is a single turn in the conversation.
type Message struct {
	Role       string     `json:"role"` // "user" | "assistant" | "tool"
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // set on role="tool"
	ToolName   string     `json:"tool_name,omitempty"`    // set on role="tool"
}

// ChatRequest is the full context sent to the LLM on each turn.
type ChatRequest struct {
	Model        string
	SystemPrompt string
	Messages     []Message
	Tools        []ToolDef
}

// ChatResponse is what the LLM returns.
type ChatResponse struct {
	Message      Message
	FinishReason string // "stop" | "tool_calls"
}

// Provider is the single interface every LLM backend must implement.
type Provider interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// StreamingProvider is implemented by providers that can stream incremental
// text as the model generates it. onDelta is called once per text fragment;
// the final ChatResponse has the same complete shape Chat would have
// returned (full content, fully-reassembled tool calls, finish reason) once
// the stream completes. Callers should type-assert for this interface and
// fall back to plain Chat when a provider doesn't implement it.
type StreamingProvider interface {
	Provider
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResponse, error)
}
