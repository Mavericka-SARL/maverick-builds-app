package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// TestAIDeveloperSetsTags: the AI Developer tags what it creates and retags
// what exists, through the same propose → confirm path as every AI write,
// including set_tags on something created earlier in the same proposal.
func TestAIDeveloperSetsTags(t *testing.T) {
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

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('TagCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Model') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	if _, err := pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID); err != nil {
		t.Fatal(err)
	}
	// Existing before the conversation: a dimension to retag by name.
	q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'region') RETURNING id::text`, modelID, revID)
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('tag-dev','dev@tag.co','Dev',$1::uuid) RETURNING id::text`, custID)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'developer',$2::uuid)`, devID, wsID); err != nil {
		t.Fatal(err)
	}

	steps := []map[string]any{
		proposeStep("create_dimension", "Create 'channel', tagged", map[string]any{"name": "channel", "tags": []string{"Go To Market"},
			"members": []map[string]any{{"code": "WEB", "label": "Web"}}}),
		proposeStep("create_metric", "Create input 'units', tagged", map[string]any{"name": "units", "is_input": true, "tags": []string{"volume"}}),
		proposeStep("create_dashboard", "Create 'Overview'", map[string]any{"name": "Overview"}),
		proposeStep("set_tags", "Tag the new dashboard", map[string]any{"kind": "dashboard", "id": ref(3), "tags": []string{"exec", "Board Pack"}}),
		proposeStep("set_tags", "Tag the existing region dimension", map[string]any{"kind": "dimension", "id": "region", "tags": []string{"geo"}}),
		proposeStep("update_metric", "Retag units", map[string]any{"metric_id": ref(2), "name": "units", "format": "number", "tags": []string{"volume", "ops"}}),
	}
	args, _ := json.Marshal(map[string]any{"steps": steps})
	fake := &multiScriptProvider{resps: []providers.ChatResponse{{
		FinishReason: "tool_calls",
		Message: providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{
			{ID: "call_1", Name: "propose_actions", Arguments: args},
		}},
	}}}

	t.Setenv("DEV_MODE", "true")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, testProvider: fake}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)
	do := func(method, path string, body any) (int, string) {
		t.Helper()
		buf, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(buf))
		req.Header.Set("X-Dev-User", "tag-dev")
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.String()
	}

	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, appID, modelID, devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled") // keeps the auto-namer off the script
	if status, body := do("POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "tag things"}); status != http.StatusOK {
		t.Fatalf("send message: %d %s", status, body)
	}
	proposals, err := aiassistant.NewProposalStore(pool).ListProposals(ctx, sess.ID)
	if err != nil || len(proposals) != 1 || proposals[0].Status != "pending" {
		t.Fatalf("want one pending proposal, got %d (err %v)", len(proposals), err)
	}
	if status, body := do("POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposals[0].ID+"/confirm", nil); status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, body)
	}
	done, _ := aiassistant.NewProposalStore(pool).GetProposal(ctx, proposals[0].ID)
	if done.Status != "executed" {
		for i, s := range done.Steps {
			t.Errorf("step %d (%s) %s: %s", i+1, s.Tool, s.Status, s.Result)
		}
		t.Fatalf("proposal %q, want executed", done.Status)
	}

	// AI writes land in the session's draft revision.
	var draft string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(draft_revision_id::text,'') FROM ai_assistant.session WHERE id=$1::uuid`, sess.ID).Scan(&draft); err != nil || draft == "" {
		t.Fatalf("draft revision: %q %v", draft, err)
	}
	for _, c := range []struct {
		table, name string
		want        []string
	}{
		{"model.dimension_def", "channel", []string{"go-to-market"}},
		{"model.dimension_def", "region", []string{"geo"}},
		{"model.metric_def", "units", []string{"volume", "ops"}},
		{"model.dashboard_def", "Overview", []string{"exec", "board-pack"}},
	} {
		var got []string
		if err := pool.QueryRow(ctx, `SELECT tags FROM `+c.table+` WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`,
			modelID, draft, c.name).Scan(&got); err != nil {
			t.Errorf("%s %s: %v", c.table, c.name, err)
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s %s tags = %v, want %v", c.table, c.name, got, c.want)
		}
	}
}
