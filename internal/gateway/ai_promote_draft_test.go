// Tests the AI Assistant's isolated-draft promotion flow end to end,
// closing a gap where clicking "Promote to Active" appeared to do nothing:
// the click did correctly flip core.model.active_revision_id (verified
// against a live instance), but nothing ever cleared the session's
// draft_revision_id — the only thing the "AI draft" banner's visibility is
// gated on — so the banner and its buttons stayed exactly as they were
// after a successful promote, giving no visible sign anything happened.
// That same missing clear meant the session's *next* confirmed proposal
// would keep reusing the (now-active) revision as if it were still an
// isolated draft, silently writing AI changes straight into the live model.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type promoteFixture struct {
	pool *pgxpool.Pool
	srv  *httptest.Server

	appID, modelID, workingRevID string
	devSub, devID                string
	otherSub, otherID            string
}

func setupPromoteFixture(t *testing.T) *promoteFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	f := &promoteFixture{pool: pool}
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

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('PromoteCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	f.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	f.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, f.appID)
	f.workingRevID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, f.workingRevID, f.modelID)

	f.devSub = "promote-dev"
	f.devID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev@promoteco.com', 'Dev', $2::uuid) RETURNING id::text`, f.devSub, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, f.devID, wsID)

	f.otherSub = "promote-other"
	f.otherID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'other@promoteco.com', 'Other', $2::uuid) RETURNING id::text`, f.otherSub, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, f.otherID, wsID)

	t.Setenv("DEV_MODE", "true")
	f.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *promoteFixture) do(t *testing.T, method, path, sub string, body any) (int, []byte) {
	t.Helper()
	var buf []byte
	if body != nil {
		var err error
		buf, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", sub)
	req.Header.Set("X-App-Id", f.appID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, respBody
}

// createSessionWithProposal seeds a chat session (owned by ownerID) and one
// pending create_dimension proposal, bypassing the LLM entirely — the
// promote flow being tested here doesn't involve one.
func (f *promoteFixture) createSessionWithProposal(t *testing.T, ownerID, dimName string) (sessionID, proposalID string) {
	t.Helper()
	ctx := context.Background()

	chatStore := aiassistant.NewChatStore(f.pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, ownerID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	params, _ := json.Marshal(map[string]string{"name": dimName})
	pStore := aiassistant.NewProposalStore(f.pool)
	proposal, err := pStore.CreateProposal(ctx, sess.ID, []aiassistant.ProposalStep{
		{Tool: "create_dimension", Description: "Create dimension '" + dimName + "'", Params: params},
	})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	return sess.ID, proposal.ID
}

func dimensionExists(t *testing.T, pool *pgxpool.Pool, modelID, revisionID, name string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM model.dimension_def
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`,
		modelID, revisionID, name).Scan(&n); err != nil {
		t.Fatalf("check dimension: %v", err)
	}
	return n > 0
}

func activeRevisionID(t *testing.T, pool *pgxpool.Pool, modelID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT active_revision_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&id); err != nil {
		t.Fatalf("load active revision: %v", err)
	}
	return id
}

func TestPromoteDraftMakesAIWritesVisibleInActiveRevision(t *testing.T) {
	f := setupPromoteFixture(t)
	sessionID, proposalID := f.createSessionWithProposal(t, f.devID, "Secret")

	// Confirm: creates an isolated draft (copy of Working) and writes the
	// dimension there — never into the active revision directly.
	status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("confirm: status %d, body %s", status, body)
	}
	var confirmResp struct {
		Session struct {
			DraftRevisionID string `json:"draft_revision_id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(body, &confirmResp); err != nil {
		t.Fatalf("parse confirm response: %v", err)
	}
	draftRevID := confirmResp.Session.DraftRevisionID
	if draftRevID == "" {
		t.Fatal("confirm did not create/report a draft revision")
	}

	if dimensionExists(t, f.pool, f.modelID, f.workingRevID, "Secret") {
		t.Error("dimension leaked into the Working revision — AI writes must stay isolated in the draft")
	}
	if !dimensionExists(t, f.pool, f.modelID, draftRevID, "Secret") {
		t.Fatal("dimension not found in the draft revision the proposal was confirmed against")
	}
	if got := activeRevisionID(t, f.pool, f.modelID); got != f.workingRevID {
		t.Fatalf("active revision changed just by confirming a proposal: %s, want unchanged %s", got, f.workingRevID)
	}

	// Promote: the model's active revision must become the draft, and the
	// session's draft_revision_id must clear (that's what the "AI draft"
	// banner's visibility is gated on in AIAssistant.tsx).
	status, body = f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/promote-draft", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("promote: status %d, body %s", status, body)
	}
	var promoteResp struct {
		Session struct {
			DraftRevisionID string `json:"draft_revision_id"`
		} `json:"session"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &promoteResp); err != nil {
		t.Fatalf("parse promote response: %v", err)
	}
	if promoteResp.Session.DraftRevisionID != "" {
		t.Errorf("session.draft_revision_id = %q after promote, want empty — the banner will never disappear otherwise", promoteResp.Session.DraftRevisionID)
	}
	if got := activeRevisionID(t, f.pool, f.modelID); got != draftRevID {
		t.Errorf("active_revision_id = %s after promote, want the draft %s", got, draftRevID)
	}

	// The exact user-facing symptom this closes: the dimension is now
	// visible under whatever the model's active revision is.
	if !dimensionExists(t, f.pool, f.modelID, activeRevisionID(t, f.pool, f.modelID), "Secret") {
		t.Error("dimension still not visible in the active revision after promotion")
	}

	// A confirmation message was posted so the chat transcript itself
	// proves promotion happened, not just a banner that silently vanished.
	found := false
	for _, m := range promoteResp.Messages {
		if m.Role == "assistant" && m.Content != "" {
			found = true
		}
	}
	if !found {
		t.Error("no confirmation message posted to the chat after promotion")
	}
}

func TestPromoteDraftGivesNextProposalAFreshDraft(t *testing.T) {
	f := setupPromoteFixture(t)
	sessionID, proposalID1 := f.createSessionWithProposal(t, f.devID, "First")

	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID1+"/confirm", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("confirm 1: status %d, body %s", status, body)
	}
	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/promote-draft", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("promote: status %d, body %s", status, body)
	}
	firstDraftRevID := activeRevisionID(t, f.pool, f.modelID)

	// A second proposal in the SAME session, confirmed after the first was
	// promoted, must isolate into a NEW draft — not silently keep writing
	// into what is now the live model.
	pStore := aiassistant.NewProposalStore(f.pool)
	params, _ := json.Marshal(map[string]string{"name": "Second"})
	proposal2, err := pStore.CreateProposal(context.Background(), sessionID, []aiassistant.ProposalStep{
		{Tool: "create_dimension", Description: "Create dimension 'Second'", Params: params},
	})
	if err != nil {
		t.Fatalf("create proposal 2: %v", err)
	}
	status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposal2.ID+"/confirm", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("confirm 2: status %d, body %s", status, body)
	}
	var confirmResp struct {
		Session struct {
			DraftRevisionID string `json:"draft_revision_id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(body, &confirmResp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	secondDraftRevID := confirmResp.Session.DraftRevisionID

	if secondDraftRevID == "" {
		t.Fatal("second confirm did not create a draft at all")
	}
	if secondDraftRevID == firstDraftRevID {
		t.Fatal("second proposal reused the promoted (now-active) revision as its draft — AI writes are no longer isolated")
	}
	if dimensionExists(t, f.pool, f.modelID, firstDraftRevID, "Second") {
		t.Error("'Second' dimension leaked into the already-promoted (live) revision")
	}
	if !dimensionExists(t, f.pool, f.modelID, secondDraftRevID, "Second") {
		t.Error("'Second' dimension not found in the fresh draft")
	}
}

func TestPromoteDraftRejectsWrongOwnerAndMissingDraft(t *testing.T) {
	f := setupPromoteFixture(t)

	t.Run("another user cannot promote someone else's session", func(t *testing.T) {
		sessionID, proposalID := f.createSessionWithProposal(t, f.devID, "Owned")
		if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil); status != http.StatusOK {
			t.Fatalf("confirm: status %d, body %s", status, body)
		}
		status, _ := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/promote-draft", f.otherSub, nil)
		if status != http.StatusNotFound {
			t.Errorf("other user promoting: status %d, want 404", status)
		}
	})

	t.Run("a session with no draft cannot be promoted", func(t *testing.T) {
		chatStore := aiassistant.NewChatStore(f.pool)
		sess, err := chatStore.CreateSession(context.Background(), f.appID, f.devID, "openai", "gpt-4o-mini")
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		status, _ := f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/promote-draft", f.devSub, nil)
		if status != http.StatusBadRequest {
			t.Errorf("promoting a draftless session: status %d, want 400", status)
		}
	})
}
