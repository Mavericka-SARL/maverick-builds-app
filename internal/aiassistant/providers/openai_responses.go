package providers

// The OpenAI Responses API (/v1/responses), for models that take function
// tools with reasoning only there: gpt-6.1-sol, gpt-6-luna and gpt-5.4-mini
// answer chat completions with 400 "Function tools with reasoning_effort are
// not supported … use /v1/responses or set reasoning_effort to 'none'", and
// at "none" a cheap model plans too shallowly to build a model (2026-11-12:
// ~470 calls of gpt-5.4-mini built nothing). A model is moved here the first
// time chat completions refuses it so; the rest stay on chat completions.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// responsesModels are the models chat completions refused with reasoning
// and tools in this process.
var responsesModels sync.Map

// needsResponses reports whether err is chat completions sending a model to
// the Responses API.
func needsResponses(err error) bool {
	return err != nil && strings.Contains(err.Error(), "/v1/responses")
}

// useResponses: the model was sent to the Responses API before, and a
// reasoning effort other than "none" is asked for.
func (p *OpenAIProvider) useResponses(model string) bool {
	if p.label != "openai" || p.apiKey == "" {
		return false
	}
	if e := p.effectiveEffort(); e == "" || e == "none" {
		return false
	}
	_, ok := responsesModels.Load(strings.ToLower(strings.TrimSpace(model)))
	return ok
}

func rememberResponses(model string) {
	responsesModels.Store(strings.ToLower(strings.TrimSpace(model)), true)
}

// responsesRequest is the wire request.
type responsesRequest struct {
	Model             string           `json:"model"`
	Instructions      string           `json:"instructions,omitempty"`
	Input             []map[string]any `json:"input"`
	Tools             []map[string]any `json:"tools,omitempty"`
	Reasoning         map[string]any   `json:"reasoning,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
	Stream            bool             `json:"stream,omitempty"`
	Store             bool             `json:"store"`
	Include           []string         `json:"include,omitempty"`
}

func (p *OpenAIProvider) buildResponsesRequest(req ChatRequest, stream bool) responsesRequest {
	in := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			in = append(in, map[string]any{"role": "user", "content": m.Content})
		case "assistant":
			// The turn's reasoning goes back with its tool calls, so a
			// reasoning model keeps its thread across the calls of a turn
			// (store is off: the encrypted items are the only copy).
			if len(m.Reasoning) > 0 {
				var items []map[string]any
				if json.Unmarshal(m.Reasoning, &items) == nil {
					in = append(in, items...)
				}
			}
			if strings.TrimSpace(m.Content) != "" {
				in = append(in, map[string]any{"role": "assistant", "content": m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := string(tc.Arguments)
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				in = append(in, map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Name, "arguments": args})
			}
		case "tool":
			out := m.Content
			if out == "" {
				out = emptyToolResult
			}
			in = append(in, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": out})
		}
	}
	r := responsesRequest{Model: req.Model, Instructions: req.SystemPrompt, Input: in, Stream: stream,
		Include: []string{"reasoning.encrypted_content"}}
	if e := p.effectiveEffort(); e != "" {
		r.Reasoning = map[string]any{"effort": e}
	}
	for _, td := range req.Tools {
		var params map[string]any
		_ = json.Unmarshal(td.Parameters, &params)
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		r.Tools = append(r.Tools, map[string]any{"type": "function", "name": td.Name, "description": td.Description,
			"parameters": params, "strict": false})
	}
	if len(r.Tools) > 0 {
		// One tool call per message, as on chat completions: only one
		// proposal is shown per turn.
		f := false
		r.ParallelToolCalls = &f
	}
	return r
}

// responsesBody is a finished response (the non-streamed answer, and the
// "response" of a streamed response.completed event).
type responsesBody struct {
	Status string `json:"status"`
	Output []struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		EncryptedContent string `json:"encrypted_content"`
		Summary          []any  `json:"summary"`
	} `json:"output"`
	Usage struct {
		InputTokens        int64 `json:"input_tokens"`
		OutputTokens       int64 `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

func (b responsesBody) chatResponse() ChatResponse {
	out := ChatResponse{Message: Message{Role: "assistant"}, FinishReason: "stop"}
	var text strings.Builder
	var reasoning []map[string]any
	for _, it := range b.Output {
		switch it.Type {
		case "message":
			for _, c := range it.Content {
				if c.Type == "output_text" {
					text.WriteString(c.Text)
				}
			}
		case "function_call":
			args := it.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{ID: it.CallID, Name: it.Name, Arguments: json.RawMessage(args)})
		case "reasoning":
			if it.EncryptedContent != "" {
				summary := it.Summary
				if summary == nil {
					summary = []any{}
				}
				reasoning = append(reasoning, map[string]any{"type": "reasoning", "encrypted_content": it.EncryptedContent, "summary": summary})
			}
		}
	}
	out.Message.Content = text.String()
	if len(reasoning) > 0 {
		out.Message.Reasoning, _ = json.Marshal(reasoning)
	}
	if len(out.Message.ToolCalls) > 0 {
		out.FinishReason = "tool_calls"
	} else if b.Status == "incomplete" {
		out.FinishReason = "length"
	}
	cached := b.Usage.InputTokensDetails.CachedTokens
	out.Usage = Usage{InputTokens: b.Usage.InputTokens - cached, CacheReadTokens: cached, OutputTokens: b.Usage.OutputTokens}
	return out
}

func (p *OpenAIProvider) postResponses(ctx context.Context, body responsesRequest) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.baseURL, "/")+"/responses", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Authorization", "Bearer "+p.apiKey)
	hreq.Header.Set("Content-Type", "application/json")
	if body.Stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := p.httpClient().Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("%s: responses: %w", p.label, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &e) == nil && e.Error.Message != "" {
			return nil, fmt.Errorf("%s: responses: status %d: %s", p.label, resp.StatusCode, e.Error.Message)
		}
		return nil, fmt.Errorf("%s: responses: status %d: %s", p.label, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (p *OpenAIProvider) chatResponses(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	resp, err := p.postResponses(ctx, p.buildResponsesRequest(req, false))
	if err != nil {
		return ChatResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var b responsesBody
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return ChatResponse{}, fmt.Errorf("%s: responses: %w", p.label, err)
	}
	if b.Error != nil && b.Error.Message != "" {
		return ChatResponse{}, fmt.Errorf("%s: responses: %s", p.label, b.Error.Message)
	}
	return b.chatResponse(), nil
}

// streamResponses reads the server-sent events of a streamed response:
// text deltas go to onDelta, a tool call's argument deltas to OnToolArgs,
// and response.completed carries the whole response, from which the result
// is built as Chat builds it.
func (p *OpenAIProvider) streamResponses(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResponse, error) {
	resp, err := p.postResponses(ctx, p.buildResponsesRequest(req, true))
	if err != nil {
		return ChatResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	names := map[string]string{} // item id → tool name
	args := map[string][]byte{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var data bytes.Buffer
	var final *responsesBody
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		defer data.Reset()
		var ev struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			ItemID   string          `json:"item_id"`
			Item     json.RawMessage `json:"item"`
			Response json.RawMessage `json:"response"`
			Message  string          `json:"message"`
			Code     string          `json:"code"`
			Error    *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(data.Bytes(), &ev) != nil {
			return nil
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				onDelta(ev.Delta)
			}
		case "response.output_item.added":
			var it struct {
				Type, ID, Name string
			}
			if json.Unmarshal(ev.Item, &it) == nil && it.Type == "function_call" {
				names[it.ID] = it.Name
			}
		case "response.function_call_arguments.delta":
			args[ev.ItemID] = append(args[ev.ItemID], ev.Delta...)
			if req.OnToolArgs != nil {
				req.OnToolArgs(names[ev.ItemID], args[ev.ItemID])
			}
		case "response.completed", "response.incomplete":
			var b responsesBody
			if err := json.Unmarshal(ev.Response, &b); err != nil {
				return fmt.Errorf("%s: responses: %w", p.label, err)
			}
			final = &b
		case "response.failed":
			var b responsesBody
			_ = json.Unmarshal(ev.Response, &b)
			if b.Error != nil && b.Error.Message != "" {
				return fmt.Errorf("%s: responses: %s", p.label, b.Error.Message)
			}
			return fmt.Errorf("%s: responses: the response failed", p.label)
		case "error":
			// The message may stand on the event or in its error object;
			// with neither, the event itself says what went wrong
			// (2026-11-12: "openai: responses: " and nothing more).
			msg := ev.Message
			if msg == "" && ev.Error != nil {
				msg = strings.TrimSpace(ev.Error.Message + " " + ev.Error.Code)
			}
			if msg == "" {
				raw := data.String()
				if len(raw) > 600 {
					raw = raw[:600] + "…"
				}
				msg = "error event: " + raw
			} else if ev.Code != "" {
				msg += " (" + ev.Code + ")"
			}
			return fmt.Errorf("%s: responses: %s", p.label, msg)
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return ChatResponse{}, err
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := sc.Err(); err != nil {
		return ChatResponse{}, fmt.Errorf("%s: responses: %w", p.label, err)
	}
	if err := flush(); err != nil {
		return ChatResponse{}, err
	}
	if final == nil {
		return ChatResponse{}, fmt.Errorf("%s: responses: the stream ended without a response", p.label)
	}
	return final.chatResponse(), nil
}
