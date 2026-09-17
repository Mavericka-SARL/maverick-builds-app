package providers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicProvider implements Provider using the official Anthropic Go SDK
// (Messages API with tool use).
type AnthropicProvider struct {
	client anthropic.Client
}

func NewAnthropic(apiKey string) *AnthropicProvider {
	return &AnthropicProvider{client: anthropic.NewClient(option.WithAPIKey(apiKey))}
}

// buildParams converts a ChatRequest into the Anthropic wire types shared by
// both the plain and streaming Messages calls.
func (p *AnthropicProvider) buildParams(req ChatRequest) anthropic.MessageNewParams {
	msgs := make([]anthropic.MessageParam, 0, len(req.Messages))

	for i := 0; i < len(req.Messages); i++ {
		m := req.Messages[i]
		switch m.Role {
		case "user":
			if m.Content == "" {
				continue
			}
			msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))

		case "assistant":
			blocks := make([]anthropic.ContentBlockParamUnion, 0, 1+len(m.ToolCalls))
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				var input any
				if err := json.Unmarshal(tc.Arguments, &input); err != nil || input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{ID: tc.ID, Name: tc.Name, Input: input},
				})
			}
			if len(blocks) == 0 {
				continue
			}
			msgs = append(msgs, anthropic.NewAssistantMessage(blocks...))

		case "tool":
			// Anthropic requires ALL tool results for one assistant turn in a
			// single user message — merge consecutive tool messages.
			results := []anthropic.ContentBlockParamUnion{
				anthropic.NewToolResultBlock(m.ToolCallID, m.Content, false),
			}
			for i+1 < len(req.Messages) && req.Messages[i+1].Role == "tool" {
				i++
				n := req.Messages[i]
				results = append(results, anthropic.NewToolResultBlock(n.ToolCallID, n.Content, false))
			}
			msgs = append(msgs, anthropic.NewUserMessage(results...))
		}
	}

	// Convert tool definitions. Our Parameters field holds a full JSON Schema
	// object; Anthropic wants properties/required unpacked into InputSchema.
	tools := make([]anthropic.ToolUnionParam, 0, len(req.Tools))
	for _, td := range req.Tools {
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		_ = json.Unmarshal(td.Parameters, &schema)
		if schema.Properties == nil {
			schema.Properties = map[string]any{}
		}
		tools = append(tools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        td.Name,
				Description: anthropic.String(td.Description),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: schema.Properties,
					Required:   schema.Required,
				},
			},
		})
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: 16000,
		Messages:  msgs,
	}
	if req.SystemPrompt != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.SystemPrompt}}
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	return params
}

// messageToChatResponse maps a complete Anthropic Message (whether obtained
// directly or accumulated from a stream) into our provider-neutral shape.
func messageToChatResponse(msg anthropic.Message) ChatResponse {
	out := ChatResponse{
		FinishReason: "stop",
		Message:      Message{Role: "assistant"},
	}
	if msg.StopReason == anthropic.StopReasonToolUse {
		out.FinishReason = "tool_calls"
	}
	for _, block := range msg.Content {
		switch variant := block.AsAny().(type) {
		case anthropic.TextBlock:
			if out.Message.Content != "" {
				out.Message.Content += "\n"
			}
			out.Message.Content += variant.Text
		case anthropic.ToolUseBlock:
			out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{
				ID:        variant.ID,
				Name:      variant.Name,
				Arguments: json.RawMessage(variant.JSON.Input.Raw()),
			})
		}
	}
	return out
}

func (p *AnthropicProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	resp, err := p.client.Messages.New(ctx, p.buildParams(req))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("anthropic: %w", err)
	}
	return messageToChatResponse(*resp), nil
}

// ChatStream mirrors Chat but calls onDelta with each incremental text
// fragment as it arrives. Tool-use input JSON also streams incrementally
// over the wire, but — same rationale as the OpenAI provider — we only
// surface human-readable text via onDelta; Message.Accumulate reassembles
// the complete tool calls into the final returned ChatResponse.
func (p *AnthropicProvider) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResponse, error) {
	stream := p.client.Messages.NewStreaming(ctx, p.buildParams(req))
	defer func() { _ = stream.Close() }()

	acc := anthropic.Message{}
	for stream.Next() {
		event := stream.Current()
		if err := acc.Accumulate(event); err != nil {
			return ChatResponse{}, fmt.Errorf("anthropic: %w", err)
		}
		if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if text := delta.Delta.Text; text != "" {
				onDelta(text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return ChatResponse{}, fmt.Errorf("anthropic: %w", err)
	}
	return messageToChatResponse(acc), nil
}
