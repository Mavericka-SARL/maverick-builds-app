package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// emptyToolResult stands in for a tool result with no text, which the Chat
// Completions API does not accept.
const emptyToolResult = "(no output)"

// OpenAIProvider implements Provider using the OpenAI Chat Completions API
// with tool calling (function calling). Also serves any OpenAI-compatible
// backend (Mistral, DeepSeek) via NewOpenAICompatible.
type OpenAIProvider struct {
	client *openai.Client
	label  string // provider name used in error messages ("openai", "mistral", ...)
}

func NewOpenAI(apiKey string) *OpenAIProvider {
	return &OpenAIProvider{client: openai.NewClient(apiKey), label: "openai"}
}

// NewOpenAICompatible targets an OpenAI-compatible chat-completions endpoint
// at a custom base URL (e.g. https://api.mistral.ai/v1).
func NewOpenAICompatible(apiKey, baseURL, label string) *OpenAIProvider {
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = baseURL
	return &OpenAIProvider{client: openai.NewClientWithConfig(cfg), label: label}
}

// buildRequest converts a ChatRequest into the go-openai wire types shared by
// both the plain and streaming completion calls.
func (p *OpenAIProvider) buildRequest(req ChatRequest) openai.ChatCompletionRequest {
	msgs := make([]openai.ChatCompletionMessage, 0, len(req.Messages)+1)

	// System message first.
	if req.SystemPrompt != "" {
		msgs = append(msgs, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleSystem,
			Content: req.SystemPrompt,
		})
	}

	// Conversation history.
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			msgs = append(msgs, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: m.Content,
			})
		case "assistant":
			if strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 {
				// An empty turn (one cut off at the stream deadline is stored
				// so) carries nothing, and OpenAI refuses an assistant
				// message with neither text nor tool calls with 400
				// "expected a string, got null" — on every later turn of the
				// session, since the history replays it.
				continue
			}
			msg := openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: m.Content,
			}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      tc.Name,
						Arguments: string(tc.Arguments),
					},
				})
			}
			msgs = append(msgs, msg)
		case "tool":
			// go-openai tags Content omitempty, so an empty tool result (a
			// read tool on an empty revision) would go out with no content,
			// which OpenAI refuses with 400 "expected a string, got null" —
			// and the stored history replays it, breaking every later turn.
			content := m.Content
			if content == "" {
				content = emptyToolResult
			}
			msgs = append(msgs, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    content,
				ToolCallID: m.ToolCallID,
			})
		}
	}

	// Build tool definitions. Parameters is typed as 'any' in go-openai, so we
	// unmarshal to map[string]any to avoid reflection issues with json.RawMessage.
	tools := make([]openai.Tool, 0, len(req.Tools))
	for _, td := range req.Tools {
		var params map[string]any
		_ = json.Unmarshal(td.Parameters, &params)
		tools = append(tools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        td.Name,
				Description: td.Description,
				Parameters:  params,
			},
		})
	}

	creq := openai.ChatCompletionRequest{
		Model:    req.Model,
		Messages: msgs,
	}
	if len(tools) > 0 {
		creq.Tools = tools
		// One tool call per message on OpenAI itself: gpt-4o-mini otherwise
		// sends the same propose_actions twice in one message, and only one
		// proposal is shown per turn. Other OpenAI-compatible vendors do not
		// all accept the field, so it is left to their defaults.
		if p.label == "openai" && !openAIReasoningModel(req.Model) {
			creq.ParallelToolCalls = false
		}
	}
	return creq
}

func (p *OpenAIProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	resp, err := p.client.CreateChatCompletion(ctx, p.buildRequest(req))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("%s: %w", p.label, err)
	}
	if len(resp.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("%s: empty response", p.label)
	}

	choice := resp.Choices[0]
	out := ChatResponse{
		FinishReason: string(choice.FinishReason),
		Message: Message{
			Role:    "assistant",
			Content: choice.Message.Content,
		},
	}
	for _, tc := range choice.Message.ToolCalls {
		out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: json.RawMessage(tc.Function.Arguments),
		})
	}
	return out, nil
}

// ChatStream mirrors Chat but calls onDelta with each incremental text
// fragment as it arrives. Tool-call arguments are NOT streamed incrementally
// (they're JSON meant for the executor, not for display) — they're
// reassembled from indexed fragments per OpenAI's streaming tool-call
// format and only appear in the final returned ChatResponse.
func (p *OpenAIProvider) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResponse, error) {
	stream, err := p.client.CreateChatCompletionStream(ctx, p.buildRequest(req))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("%s: %w", p.label, err)
	}
	defer func() { _ = stream.Close() }()

	var content []byte
	finishReason := ""
	type toolCallAccum struct {
		id, name string
		args     []byte
	}
	byIndex := map[int]*toolCallAccum{}
	var order []int

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ChatResponse{}, fmt.Errorf("%s: %w", p.label, err)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			finishReason = string(choice.FinishReason)
		}
		if choice.Delta.Content != "" {
			content = append(content, choice.Delta.Content...)
			onDelta(choice.Delta.Content)
		}
		for _, tc := range choice.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			acc, ok := byIndex[idx]
			if !ok {
				acc = &toolCallAccum{}
				byIndex[idx] = acc
				order = append(order, idx)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.args = append(acc.args, tc.Function.Arguments...)
		}
	}

	out := ChatResponse{
		FinishReason: finishReason,
		Message:      Message{Role: "assistant", Content: string(content)},
	}
	for _, idx := range order {
		acc := byIndex[idx]
		out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: json.RawMessage(acc.args),
		})
	}
	return out, nil
}

// openAIReasoningModel reports the o-series (o1, o3, o4-mini, …), which
// refuse parallel_tool_calls with a 400.
func openAIReasoningModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return len(m) >= 2 && m[0] == 'o' && m[1] >= '0' && m[1] <= '9'
}
