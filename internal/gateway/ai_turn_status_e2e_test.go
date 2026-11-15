package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// stagedProvider streams its scripted answers as a reasoning model does:
// accepted, reasoning in silence for a while, then the output.
type stagedProvider struct {
	multiScriptProvider
	think time.Duration
}

func (p *stagedProvider) ChatStream(ctx context.Context, req providers.ChatRequest, onDelta func(string)) (providers.ChatResponse, error) {
	if r, ok := sideCall(req); ok {
		return r, nil
	}
	if req.OnStage != nil {
		req.OnStage(providers.StageConnected)
		req.OnStage(providers.StageReasoning)
	}
	select {
	case <-time.After(p.think):
	case <-ctx.Done():
		return providers.ChatResponse{}, ctx.Err()
	}
	resp, _ := p.Chat(ctx, req)
	for _, tc := range resp.Message.ToolCalls {
		if req.OnToolArgs != nil {
			req.OnToolArgs(tc.Name, tc.Arguments)
		}
	}
	if resp.Message.Content != "" {
		onDelta(resp.Message.Content)
	}
	return resp, nil
}

// A turn tells the console, through "status" events, which step it is on:
// each AI call numbered as the session's usage counts it, the provider's
// stages while the model reasons in silence — repeated by the beat — then
// the tool on the server, with the steps before it.
func TestSendMessage_StatusFollowsTheTurn(t *testing.T) {
	old := statusBeat
	statusBeat = 40 * time.Millisecond
	t.Cleanup(func() { statusBeat = old })

	usage := providers.Usage{InputTokens: 1000, OutputTokens: 10}
	fake := &stagedProvider{think: 300 * time.Millisecond, multiScriptProvider: multiScriptProvider{resps: []providers.ChatResponse{
		{FinishReason: "tool_calls", Usage: usage, Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: "t1", Name: "list_metrics", Arguments: json.RawMessage(`{}`)}}}},
		{FinishReason: "stop", Usage: usage, Message: providers.Message{Role: "assistant", Content: "The model has no metrics yet."}},
	}}}
	env := newAITurnEnv(t, fake)
	code, sse := env.send("How many metrics are there? Change nothing.")
	if code != http.StatusOK || !strings.Contains(sse, "event: done") {
		t.Fatalf("send: %d\n%s", code, sse)
	}

	type st struct {
		Phase   string         `json:"phase"`
		Label   string         `json:"label"`
		Call    int            `json:"call"`
		Stage   string         `json:"stage"`
		Tokens  int            `json:"context_tokens"`
		IdleS   int            `json:"idle_limit_s"`
		Recent  []turnStepDone `json:"recent"`
		Attempt int            `json:"attempt"`
	}
	var got []st
	var trail []string
	for _, frame := range strings.Split(sse, "\n\n") {
		if !strings.HasPrefix(frame, "event: status\n") {
			continue
		}
		var s st
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "event: status\ndata: ")), &s); err != nil {
			t.Fatalf("status %q: %v", frame, err)
		}
		got = append(got, s)
		step := s.Phase + ":" + s.Label
		if s.Phase == "ai" {
			step = "ai:" + s.Stage
		}
		if len(trail) == 0 || trail[len(trail)-1] != step {
			trail = append(trail, step)
		}
	}
	t.Logf("status trail: %v", trail)

	// How the request is read, then the two calls, each through its
	// stages, with the tool between them.
	want := []string{"server:read as a direct order: doing exactly what it names", "ai:sending", "ai:connected", "ai:reasoning", "ai:writing", "server:going through the AI's answer",
		"server:list_metrics", "ai:sending", "ai:connected", "ai:reasoning", "ai:writing", "server:going through the AI's answer"}
	if strings.Join(trail, " > ") != strings.Join(want, " > ") {
		t.Fatalf("status trail\n got %v\nwant %v", trail, want)
	}
	beats, calls := 0, map[int]bool{}
	for _, s := range got {
		if s.Phase == "ai" {
			calls[s.Call] = true
			if s.Stage == "reasoning" {
				beats++
			}
			if s.Tokens <= 0 || s.IdleS != int(aiCallIdleLimit().Seconds()) || s.Attempt != 1 {
				t.Errorf("call status %+v", s)
			}
		}
	}
	if !calls[1] || !calls[2] || len(calls) != 2 {
		t.Errorf("call numbers %v, want 1 and 2", calls)
	}
	// 300 ms of silent reasoning, beaten every 40 ms: repeated, not sent once.
	if beats < 6 {
		t.Errorf("%d reasoning statuses — the beat did not repeat them", beats)
	}
	last := got[len(got)-1]
	var labels []string
	for _, r := range last.Recent {
		labels = append(labels, r.Label)
	}
	if strings.Join(labels, ",") != "AI call 1,going through the AI's answer,list_metrics,AI call 2" {
		t.Errorf("steps before: %v", labels)
	}
	for _, r := range last.Recent {
		if strings.HasPrefix(r.Label, "AI call") && r.MS < 300 {
			t.Errorf("%s took %d ms, reasoned 300", r.Label, r.MS)
		}
	}
	// Nothing is sent after the turn's last event.
	if i, j := strings.LastIndex(sse, "event: status"), strings.LastIndex(sse, "event: done"); i > j {
		t.Errorf("a status after done")
	}
}
