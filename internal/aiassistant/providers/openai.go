package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	openai "github.com/sashabaranov/go-openai"
)

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
			msgs = append(msgs, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    m.Content,
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
