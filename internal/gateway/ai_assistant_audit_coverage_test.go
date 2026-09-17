// AI Assistant (/api/ai/...) was entirely unaudited except revision.activated
// (aiPromoteDraft). This covers session/document/proposal/settings actions —
// deliberately not individual chat messages (would flood the 200-row-capped
// admin audit view with conversational noise without adding signal).
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type aiAuditFixture struct {
	pool           *pgxpool.Pool
	srv            *httptest.Server
	appID, modelID string
	workingRevID   string
	devSub, devID  string
}

func setupAIAuditFixture(t *testing.T) *aiAuditFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	f := &aiAuditFixture{pool: pool}
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

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('AIAuditCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	f.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	f.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, f.appID)
	f.workingRevID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, f.workingRevID, f.modelID)

	f.devSub = "ai-audit-dev"
	f.devID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev@aiaudit.com', 'Dev', $2::uuid) RETURNING id::text`, f.devSub, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, f.devID, wsID)

	t.Setenv("DEV_MODE", "true")
	f.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *aiAuditFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
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
	req.Header.Set("X-Dev-User", f.devSub)
	req.Header.Set("X-App-Id", f.appID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

func (f *aiAuditFixture) uploadDocument(t *testing.T, sessionID string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte("hello world")); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), "POST", f.srv.URL+"/api/ai/sessions/"+sessionID+"/documents", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", f.devSub)
	req.Header.Set("X-App-Id", f.appID)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

func (f *aiAuditFixture) latestAuditEvent(t *testing.T, eventType string) auditRow {
	t.Helper()
	var row auditRow
	err := f.pool.QueryRow(context.Background(), `
		SELECT category::text, event_type, resource_type, resource_id,
		       application_id::text, revision_id::text
		FROM audit.audit_event
		WHERE event_type = $1
		ORDER BY occurred_at DESC
		LIMIT 1
	`, eventType).Scan(&row.category, &row.eventType, &row.resourceType, &row.resourceID, &row.applicationID, &row.revisionID)
	if err != nil {
		t.Fatalf("no audit row for event_type %q: %v", eventType, err)
	}
	return row
}

// createSession seeds a chat session owned by devID, bypassing the LLM.
func (f *aiAuditFixture) createSession(t *testing.T) string {
	t.Helper()
	sess, err := aiassistant.NewChatStore(f.pool).CreateSession(context.Background(), f.appID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess.ID
}

func (f *aiAuditFixture) createProposal(t *testing.T, sessionID, dimName string) string {
	t.Helper()
	params, _ := json.Marshal(map[string]string{"name": dimName})
	proposal, err := aiassistant.NewProposalStore(f.pool).CreateProposal(context.Background(), sessionID, []aiassistant.ProposalStep{
		{Tool: "create_dimension", Description: "Create dimension '" + dimName + "'", Params: params},
	})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	return proposal.ID
}

func TestAIAssistantMutationsAreAudited(t *testing.T) {
	f := setupAIAuditFixture(t)

	// ── proposal confirm ────────────────────────────────────────────────
	sessionID := f.createSession(t)
	proposalID := f.createProposal(t, sessionID, "Secret")
	status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", nil)
	if status != http.StatusOK {
		t.Fatalf("confirm proposal: status=%d body=%v", status, body)
	}
	if row := f.latestAuditEvent(t, "ai_proposal.confirmed"); row.resourceID != proposalID || row.category != "ai_assistant" {
		t.Errorf("ai_proposal.confirmed row = %+v, want resource_id=%s category=ai_assistant", row, proposalID)
	}

	// ── proposal reject ─────────────────────────────────────────────────
	proposalID2 := f.createProposal(t, sessionID, "Region2")
	status, _ = f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID2+"/reject", nil)
	if status != http.StatusOK {
		t.Fatalf("reject proposal: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "ai_proposal.rejected"); row.resourceID != proposalID2 {
		t.Errorf("ai_proposal.rejected resource_id = %s, want %s", row.resourceID, proposalID2)
	}

	// ── document upload / delete ────────────────────────────────────────
	status, body = f.uploadDocument(t, sessionID)
	if status != http.StatusOK {
		t.Fatalf("upload document: status=%d body=%v", status, body)
	}
	docID, _ := body["id"].(string)
	if docID == "" {
		t.Fatalf("expected document id, got %v", body)
	}
	f.latestAuditEvent(t, "ai_document.uploaded")

	status, _ = f.do(t, "DELETE", "/api/ai/sessions/"+sessionID+"/documents/"+docID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete document: status=%d", status)
	}
	f.latestAuditEvent(t, "ai_document.deleted")

	// ── session delete ──────────────────────────────────────────────────
	status, _ = f.do(t, "DELETE", "/api/ai/sessions/"+sessionID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete session: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "ai_session.deleted"); row.resourceID != sessionID {
		t.Errorf("ai_session.deleted resource_id = %s, want %s", row.resourceID, sessionID)
	}

	// ── settings update ─────────────────────────────────────────────────
	status, _ = f.do(t, "PUT", "/api/ai/settings", map[string]string{"provider": "openai", "model": "gpt-4o-mini"})
	if status != http.StatusOK {
		t.Fatalf("update settings: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "ai_settings.updated"); row.category != "ai_assistant" {
		t.Errorf("ai_settings.updated category = %s, want ai_assistant", row.category)
	}

	// ── settings test (no API key available -> ok:false, still audited) ──
	status, body = f.do(t, "POST", "/api/ai/settings/test", map[string]string{"provider": "openai", "model": "gpt-4o-mini"})
	if status != http.StatusOK {
		t.Fatalf("test settings: status=%d body=%v", status, body)
	}
	f.latestAuditEvent(t, "ai_settings.tested")
}
