// Tests that an AI Assistant session belongs to one model. Reported live: a
// developer working in a second model ("Test") asked the assistant to build
// it, and the assistant answered from the sign-up tour model — the
// application's default — because sessions belonged to the application and
// the chat send never named the console's revision.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// promptCapturingProvider answers every turn with plain text and keeps the
// system prompt of the last call — the prompt names the model the turn
// was scoped to.
type promptCapturingProvider struct {
	lastSystemPrompt string
}

func (p *promptCapturingProvider) Chat(_ context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	p.lastSystemPrompt = req.SystemPrompt
	return providers.ChatResponse{
		FinishReason: "stop",
		Message:      providers.Message{Role: "assistant", Content: "ok"},
	}, nil
}

func TestAISession_FollowsTheConsoleModel(t *testing.T) {
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

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('TwoModelCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Getting started', 'planning') RETURNING id::text`, wsID, custID)
	newModel := func(name string) (modelID, revID string) {
		modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, appID, name)
		revID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'first') RETURNING id::text`, modelID)
		exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='first' WHERE id=$2::uuid`, revID, modelID)
		return modelID, revID
	}
	tourModel, _ := newModel("Learn the platform")
	testModel, testRev := newModel("Test")
	exec(`UPDATE core.application SET default_model_id=$1::uuid WHERE id=$2::uuid`, tourModel, appID)

	devSub := "twomodel-dev"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev@twomodelco.com', 'Dev', $2::uuid) RETURNING id::text`, devSub, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, devID, wsID)

	fake := &promptCapturingProvider{}
	t.Setenv("DEV_MODE", "true")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, testProvider: fake}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)

	call := func(method, path string, body any) (int, string) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, rd)
		req.Header.Set("X-Dev-User", devSub)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	at := func(path, revID string) string {
		if revID == "" {
			return path
		}
		return path + "?revision_id=" + revID
	}
	chatStore := aiassistant.NewChatStore(pool)
	newSession := func(revID string) aiassistant.Session {
		t.Helper()
		status, body := call("POST", at("/api/ai/sessions", revID), nil)
		if status != http.StatusOK {
			t.Fatalf("create session (revision %q): %d %s", revID, status, body)
		}
		var sess aiassistant.Session
		_ = json.Unmarshal([]byte(body), &sess)
		if err := chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled: no background namer call"); err != nil {
			t.Fatalf("pre-title session: %v", err)
		}
		return sess
	}
	listed := func(revID string) []string {
		t.Helper()
		status, body := call("GET", at("/api/ai/sessions", revID), nil)
		if status != http.StatusOK {
			t.Fatalf("list sessions (revision %q): %d %s", revID, status, body)
		}
		var list []aiassistant.Session
		_ = json.Unmarshal([]byte(body), &list)
		ids := make([]string, 0, len(list))
		for _, s := range list {
			ids = append(ids, s.ID)
		}
		return ids
	}
	send := func(sess aiassistant.Session, revID string) (int, string) {
		return call("POST", at("/api/ai/sessions/"+sess.ID+"/messages", revID), map[string]string{"content": "what model is this?"})
	}

	// A session started from Test's revision belongs to Test, is listed
	// there only, and works in Test.
	testSess := newSession(testRev)
	if testSess.ModelID != testModel {
		t.Fatalf("session started from Test's revision has model %s, want Test (%s)", testSess.ModelID, testModel)
	}
	if got := listed(testRev); len(got) != 1 || got[0] != testSess.ID {
		t.Fatalf("Test's sessions = %v, want just %s", got, testSess.ID)
	}
	if got := listed(""); len(got) != 0 {
		t.Fatalf("the default model lists Test's session: %v", got)
	}
	if status, body := send(testSess, testRev); status != http.StatusOK {
		t.Fatalf("send in Test: %d %s", status, body)
	}
	if !strings.Contains(fake.lastSystemPrompt, "Model: **Test**") {
		t.Fatalf("a Test session should work in Test; prompt:\n%s", fake.lastSystemPrompt)
	}

	// From the default model it is refused, by name — not quietly answered
	// from the tour, as reported.
	status, body := send(testSess, "")
	if status != http.StatusConflict || !strings.Contains(body, `the model \"Test\"`) {
		t.Fatalf("send to a Test session from the default model: got %d %s, want 409 naming Test", status, body)
	}

	// A tour session is refused from Test on both the chat and the confirm
	// path, and the refused confirm writes nothing.
	tourSess := newSession("")
	if tourSess.ModelID != tourModel {
		t.Fatalf("session started without a revision has model %s, want the default (%s)", tourSess.ModelID, tourModel)
	}
	status, body = send(tourSess, testRev)
	if status != http.StatusConflict || !strings.Contains(body, "Learn the platform") {
		t.Fatalf("send to a tour session from Test: got %d %s, want 409 naming the tour model", status, body)
	}
	dimParams, _ := json.Marshal(map[string]any{"name": "Region"})
	proposal, err := aiassistant.NewProposalStore(pool).CreateProposal(ctx, tourSess.ID, []aiassistant.ProposalStep{
		{Tool: "create_dimension", Description: "Create dimension 'Region'", Params: dimParams},
	}, nil)
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	status, body = call("POST", at("/api/ai/sessions/"+tourSess.ID+"/proposals/"+proposal.ID+"/confirm", testRev), nil)
	if status != http.StatusConflict || !strings.Contains(body, "Learn the platform") {
		t.Fatalf("confirm a tour proposal from Test: got %d %s, want 409 naming the tour model", status, body)
	}
	var written int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.dimension_def WHERE model_id IN ($1::uuid, $2::uuid)`, tourModel, testModel).Scan(&written)
	if written != 0 {
		t.Fatalf("refused confirm still wrote %d dimension row(s)", written)
	}
	var drafts int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.revision WHERE model_id IN ($1::uuid, $2::uuid) AND name LIKE 'AI Draft%'`, tourModel, testModel).Scan(&drafts)
	if drafts != 0 {
		t.Fatalf("refused confirm still created %d draft revision(s)", drafts)
	}

	// Confirmed from its own model, the proposal lands in a draft of it.
	status, body = call("POST", "/api/ai/sessions/"+tourSess.ID+"/proposals/"+proposal.ID+"/confirm", nil)
	if status != http.StatusOK {
		t.Fatalf("confirm a tour proposal from the tour: %d %s", status, body)
	}
	_ = pool.QueryRow(ctx, `
		SELECT count(*) FROM model.dimension_def d JOIN ai_assistant.session s ON s.draft_revision_id = d.revision_id
		WHERE s.id = $1::uuid AND d.model_id = $2::uuid`, tourSess.ID, tourModel).Scan(&written)
	if written != 1 {
		t.Fatalf("confirm from the tour wrote %d dimension row(s) into the tour's draft, want 1", written)
	}
}
