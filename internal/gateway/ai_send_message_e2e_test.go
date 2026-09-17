// Tests aiSendMessage's real SSE tool-call loop end to end — every other
// AI Assistant test in this repo seeds a proposal directly via
// aiassistant.NewProposalStore and skips straight to /confirm, so the
// actual POST /api/ai/sessions/{id}/messages path (the LLM call, the
// propose_actions tool-call detection, the SSE "proposal" event, and the
// proposal actually landing in the DB) has never been exercised by any
// test. Uses handler.testProvider — a package-private seam that makes
// buildProviderForRequest return a scripted fake instead of resolving
// DB-stored settings and constructing a real network client — to script a
// deterministic tool-calling response without hitting a live LLM API.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// scriptedProvider is a fake providers.Provider that always returns the
// same canned response, regardless of what it's asked — enough to prove
// the propose_actions path works without needing a real multi-turn script.
type scriptedProvider struct {
	calls int
	resp  providers.ChatResponse
}

func (p *scriptedProvider) Chat(_ context.Context, _ providers.ChatRequest) (providers.ChatResponse, error) {
	p.calls++
	return p.resp, nil
}

func TestSendMessage_ProposeActionsToolCallCreatesRealProposal(t *testing.T) {
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('SendMsgCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID)

	devSub := "sendmsg-dev"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev@sendmsgco.com', 'Dev', $2::uuid) RETURNING id::text`, devSub, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, devID, wsID)

	// The LLM's canned response: a propose_actions tool call proposing one
	// create_dimension step — mirrors the exact args shape the real
	// prompt.go examples teach the model to emit.
	proposeArgs, _ := json.Marshal(map[string]any{
		"steps": []map[string]any{
			{
				"tool":        "create_dimension",
				"description": "Create dimension 'Region'",
				"params":      map[string]any{"name": "Region"},
			},
		},
	})
	fake := &scriptedProvider{
		resp: providers.ChatResponse{
			FinishReason: "tool_calls",
			Message: providers.Message{
				Role: "assistant",
				ToolCalls: []providers.ToolCall{
					{ID: "call_1", Name: "propose_actions", Arguments: proposeArgs},
				},
			},
		},
	}

	t.Setenv("DEV_MODE", "true")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, testProvider: fake}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)

	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, appID, devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// Pre-title the session: the background auto-namer fires one extra
	// Chat() on a session's FIRST message when the title is empty, which
	// would break this test's "exactly 1 call" assertion about the tool
	// loop itself (auto-naming has its own test in internal/aiassistant).
	if err := chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled for loop-count purity"); err != nil {
		t.Fatalf("pre-title session: %v", err)
	}

	reqBody, _ := json.Marshal(map[string]string{"content": "add a Region dimension"})
	req, err := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/ai/sessions/"+sess.ID+"/messages", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", devSub)
	req.Header.Set("X-App-Id", appID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read SSE body: %v", err)
	}
	sseText := body.String()

	if fake.calls != 1 {
		t.Errorf("provider.Chat called %d time(s), want exactly 1 (propose_actions ends the loop immediately)", fake.calls)
	}

	// Parse the "proposal" SSE event's data payload.
	const marker = "event: proposal\ndata: "
	idx := strings.Index(sseText, marker)
	if idx < 0 {
		t.Fatalf("no 'proposal' SSE event found in response:\n%s", sseText)
	}
	rest := sseText[idx+len(marker):]
	end := strings.Index(rest, "\n\n")
	if end < 0 {
		end = len(rest)
	}
	var evt struct {
		Proposal struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Steps  []struct {
				Tool string `json:"tool"`
			} `json:"steps"`
		} `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(rest[:end]), &evt); err != nil {
		t.Fatalf("parse proposal event data: %v\nraw: %s", err, rest[:end])
	}
	if evt.Proposal.Status != "pending" {
		t.Errorf("proposal.status = %q, want pending", evt.Proposal.Status)
	}
	if len(evt.Proposal.Steps) != 1 || evt.Proposal.Steps[0].Tool != "create_dimension" {
		t.Fatalf("unexpected proposal steps: %+v", evt.Proposal.Steps)
	}

	// The proposal must be a REAL row a subsequent /confirm can act on, not
	// just an SSE payload — this is the exact thing every other AI test
	// bypasses by seeding a proposal directly.
	pStore := aiassistant.NewProposalStore(pool)
	stored, err := pStore.GetProposal(ctx, evt.Proposal.ID)
	if err != nil {
		t.Fatalf("load stored proposal: %v", err)
	}
	if stored.SessionID != sess.ID {
		t.Errorf("stored proposal session_id = %q, want %q", stored.SessionID, sess.ID)
	}

	// The assistant's tool-call message and the placeholder tool-result
	// message must also be persisted, so the chat transcript itself shows
	// the proposal was made.
	msgs, err := chatStore.ListMessages(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	var sawAssistantToolCall, sawToolResult bool
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			sawAssistantToolCall = true
		}
		if m.Role == "tool" {
			sawToolResult = true
		}
	}
	if !sawAssistantToolCall {
		t.Error("no assistant message with tool_calls was saved")
	}
	if !sawToolResult {
		t.Error("no tool-role placeholder message was saved")
	}
}

// multiScriptProvider returns a different canned response per call — enough
// to script "model proposes 60 steps → server rejects → model re-proposes 50".
type multiScriptProvider struct {
	calls int
	resps []providers.ChatResponse
}

func (p *multiScriptProvider) Chat(_ context.Context, _ providers.ChatRequest) (providers.ChatResponse, error) {
	p.calls++
	if p.calls > len(p.resps) {
		return p.resps[len(p.resps)-1], nil
	}
	return p.resps[p.calls-1], nil
}

// TestSendMessage_OversizedProposalRejectedThenBatched: server-side proposal
// batching. A propose_actions call with more than 50 steps must NOT become a
// proposal — the constraint goes back to the model as the tool result, and
// the model's follow-up batch of exactly 50 becomes the proposal the
// developer sees. Bulk jobs (500-member re-parents) otherwise ride on the
// model's output limit and truncate mid-JSON on smaller models.
func TestSendMessage_OversizedProposalRejectedThenBatched(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('BatchCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID)
	devSub := "batch-dev"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev@batchco.com', 'Dev', $2::uuid) RETURNING id::text`, devSub, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, devID, wsID)

	mkSteps := func(n int) json.RawMessage {
		steps := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			steps = append(steps, map[string]any{
				"tool":        "create_metric",
				"description": fmt.Sprintf("Create metric m%d", i),
				"params":      map[string]any{"name": fmt.Sprintf("m%d", i), "is_input": true},
			})
		}
		raw, _ := json.Marshal(map[string]any{"steps": steps})
		return raw
	}
	toolResp := func(callID string, args json.RawMessage) providers.ChatResponse {
		return providers.ChatResponse{
			FinishReason: "tool_calls",
			Message: providers.Message{
				Role:      "assistant",
				ToolCalls: []providers.ToolCall{{ID: callID, Name: "propose_actions", Arguments: args}},
			},
		}
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		toolResp("call_big", mkSteps(60)),   // rejected server-side
		toolResp("call_batch", mkSteps(50)), // accepted
	}}

	t.Setenv("DEV_MODE", "true")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, testProvider: fake}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)

	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, appID, devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled"); err != nil {
		t.Fatalf("pre-title: %v", err)
	}

	reqBody, _ := json.Marshal(map[string]string{"content": "create 60 metrics"})
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/ai/sessions/"+sess.ID+"/messages", bytes.NewReader(reqBody))
	req.Header.Set("X-Dev-User", devSub)
	req.Header.Set("X-App-Id", appID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(resp.Body)
	sseText := body.String()

	if fake.calls != 2 {
		t.Errorf("provider.Chat called %d time(s), want 2 (oversized rejected, then the 50-step batch)", fake.calls)
	}
	if !strings.Contains(sseText, "event: proposal") {
		t.Fatalf("no proposal event — the batched retry never became a proposal:\n%s", sseText)
	}
	// The stored transcript carries the rejection tool result for call_big.
	msgs, _ := chatStore.ListMessages(ctx, sess.ID)
	rejected := false
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, "Proposal rejected: 60 steps") {
			rejected = true
		}
	}
	if !rejected {
		t.Error("no 'Proposal rejected' tool message in the transcript — the constraint never reached the model")
	}
	// The proposal that exists has exactly 50 steps.
	var stepCount int
	if err := pool.QueryRow(ctx, `
		SELECT jsonb_array_length(steps) FROM ai_assistant.proposal p WHERE p.session_id=$1::uuid
	`, sess.ID).Scan(&stepCount); err != nil {
		t.Fatalf("read proposal steps: %v", err)
	}
	if stepCount != 50 {
		t.Errorf("stored proposal has %d steps, want 50", stepCount)
	}
}
