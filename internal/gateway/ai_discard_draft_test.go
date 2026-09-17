// Tests the AI Assistant's discard-draft flow end to end, mirroring
// ai_promote_draft_test.go's fixture shape. Closes the real bug the old
// frontend flow had: calling the generic DELETE /api/developer/revisions/{id}
// and only updating local React state afterward never cleared
// session.draft_revision_id server-side, so the session's NEXT confirmed
// proposal reused the (now-deleted) draft's UUID and 500'd on an FK
// violation instead of starting fresh.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

func TestDiscardDraftDeletesTheDraftAndClearsSessionState(t *testing.T) {
	f := setupPromoteFixture(t)
	sessionID, proposalID := f.createSessionWithProposal(t, f.devID, "ScratchDim")

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

	status, body = f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/discard-draft", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("discard: status %d, body %s", status, body)
	}
	var discardResp struct {
		Session struct {
			DraftRevisionID string `json:"draft_revision_id"`
		} `json:"session"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &discardResp); err != nil {
		t.Fatalf("parse discard response: %v", err)
	}
	if discardResp.Session.DraftRevisionID != "" {
		t.Errorf("session.draft_revision_id = %q after discard, want empty", discardResp.Session.DraftRevisionID)
	}

	var revCount int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM model.revision WHERE id=$1::uuid`, draftRevID).Scan(&revCount); err != nil {
		t.Fatalf("count draft revision row: %v", err)
	}
	if revCount != 0 {
		t.Errorf("draft revision row still exists after discard, want it deleted")
	}
	if dimensionExists(t, f.pool, f.modelID, draftRevID, "ScratchDim") {
		t.Error("dimension from the discarded draft still exists — ON DELETE CASCADE should have removed it")
	}

	found := false
	for _, m := range discardResp.Messages {
		if m.Role == "assistant" && m.Content != "" {
			found = true
		}
	}
	if !found {
		t.Error("no confirmation message posted to the chat after discard")
	}
}

// TestDiscardDraftThenNextProposalStartsFresh is the actual bug this batch
// fixes: before aiDiscardDraft existed, the frontend deleted the draft
// revision directly and only cleared draft_revision_id in local React
// state — the session row server-side kept pointing at the deleted UUID, so
// the next confirmed proposal in the same session tried to write into a
// revision that no longer existed and 500'd on an FK violation instead of
// lazily creating a new draft.
func TestDiscardDraftThenNextProposalStartsFresh(t *testing.T) {
	f := setupPromoteFixture(t)
	sessionID, proposalID1 := f.createSessionWithProposal(t, f.devID, "First")

	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID1+"/confirm", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("confirm 1: status %d, body %s", status, body)
	}
	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/discard-draft", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("discard: status %d, body %s", status, body)
	}

	params, _ := json.Marshal(map[string]string{"name": "Second"})
	pStore := aiassistant.NewProposalStore(f.pool)
	proposal2, err := pStore.CreateProposal(context.Background(), sessionID, []aiassistant.ProposalStep{
		{Tool: "create_dimension", Description: "Create dimension 'Second'", Params: params},
	})
	if err != nil {
		t.Fatalf("create proposal 2: %v", err)
	}
	status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposal2.ID+"/confirm", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("confirm 2 (post-discard): status %d, body %s — this is exactly the dangling draft_revision_id / FK violation bug", status, body)
	}
	var confirmResp struct {
		Session struct {
			DraftRevisionID string `json:"draft_revision_id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(body, &confirmResp); err != nil {
		t.Fatalf("parse confirm 2 response: %v", err)
	}
	newDraftRevID := confirmResp.Session.DraftRevisionID
	if newDraftRevID == "" {
		t.Fatal("post-discard confirm did not create a fresh draft")
	}
	if !dimensionExists(t, f.pool, f.modelID, newDraftRevID, "Second") {
		t.Error("'Second' dimension not found in the fresh post-discard draft")
	}
}

func TestDiscardDraftRejectsWrongOwnerAndMissingDraft(t *testing.T) {
	f := setupPromoteFixture(t)

	t.Run("another user cannot discard someone else's session", func(t *testing.T) {
		sessionID, proposalID := f.createSessionWithProposal(t, f.devID, "Owned")
		if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil); status != http.StatusOK {
			t.Fatalf("confirm: status %d, body %s", status, body)
		}
		status, _ := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/discard-draft", f.otherSub, nil)
		if status != http.StatusNotFound {
			t.Errorf("other user discarding: status %d, want 404", status)
		}
	})

	t.Run("a session with no draft cannot be discarded", func(t *testing.T) {
		chatStore := aiassistant.NewChatStore(f.pool)
		sess, err := chatStore.CreateSession(context.Background(), f.appID, f.devID, "openai", "gpt-4o-mini")
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		status, _ := f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/discard-draft", f.devSub, nil)
		if status != http.StatusBadRequest {
			t.Errorf("discarding a draftless session: status %d, want 400", status)
		}
	})
}

// TestListProposalsEndpoint_ReturnsFullHistoryWithSummary is the HTTP-level
// counterpart to TestListProposals_ReturnsEveryStatusMostRecentFirst
// (internal/aiassistant/proposal_store_test.go) — proves the Activity
// panel's actual endpoint returns every proposal status (not just pending)
// and that each row carries the human-readable summary text the frontend
// renders directly instead of re-deriving from raw step JSON.
func TestListProposalsEndpoint_ReturnsFullHistoryWithSummary(t *testing.T) {
	f := setupPromoteFixture(t)
	sessionID, proposalID := f.createSessionWithProposal(t, f.devID, "Tracked")

	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("confirm: status %d, body %s", status, body)
	}

	params, _ := json.Marshal(map[string]string{"name": "Pending2"})
	pStore := aiassistant.NewProposalStore(f.pool)
	if _, err := pStore.CreateProposal(context.Background(), sessionID, []aiassistant.ProposalStep{
		{Tool: "create_dimension", Description: "Create dimension 'Pending2'", Params: params},
	}); err != nil {
		t.Fatalf("create second (pending) proposal: %v", err)
	}

	status, body := f.do(t, "GET", "/api/ai/sessions/"+sessionID+"/proposals", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("list proposals: status %d, body %s", status, body)
	}
	var proposals []struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(body, &proposals); err != nil {
		t.Fatalf("parse list response: %v", err)
	}
	if len(proposals) != 2 {
		t.Fatalf("expected 2 proposals (executed + pending), got %d: %+v", len(proposals), proposals)
	}
	for _, p := range proposals {
		if p.Summary == "" {
			t.Errorf("proposal %s (status %s) has an empty summary", p.ID, p.Status)
		}
		if p.Status == "executed" && p.Summary == "Awaiting confirmation." {
			t.Errorf("executed proposal got the pending summary text: %q", p.Summary)
		}
	}

	// Wrong owner gets 404, matching every other session-scoped AI route.
	status, _ = f.do(t, "GET", "/api/ai/sessions/"+sessionID+"/proposals", f.otherSub, nil)
	if status != http.StatusNotFound {
		t.Errorf("other user listing proposals: status %d, want 404", status)
	}
}
