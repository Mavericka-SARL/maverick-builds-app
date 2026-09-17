// Tests two AI Assistant confirm-path edge cases that no prior test
// exercised through the real HTTP endpoint: a proposal where one step fails
// and one succeeds (status must land "partial", the draft must stay usable
// for the next proposal, and the failure must be individually visible), and
// the 409 conflict both confirm and reject return for an
// already-processed proposal.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// TestConfirmProposal_PartialFailureLeavesDraftUsable proposes two steps —
// a valid create_dimension and a create_metric whose formula references a
// dimension/metric that doesn't exist — and confirms the mixed outcome:
// proposal.status="partial", the successful step's resource is really
// there, the failed step's result names what went wrong, and — critically —
// the draft revision itself isn't poisoned: a second, all-valid proposal in
// the same session still confirms cleanly afterward.
func TestConfirmProposal_PartialFailureLeavesDraftUsable(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()

	chatStore := aiassistant.NewChatStore(f.pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	dimParams, _ := json.Marshal(map[string]any{"name": "Region"})
	badMetricParams, _ := json.Marshal(map[string]any{
		"name": "bogus_calc", "is_input": false, "formula": "totally_nonexistent_ref",
	})
	pStore := aiassistant.NewProposalStore(f.pool)
	proposal, err := pStore.CreateProposal(ctx, sess.ID, []aiassistant.ProposalStep{
		{Tool: "create_dimension", Description: "Create dimension 'Region'", Params: dimParams},
		{Tool: "create_metric", Description: "Create calc metric 'bogus_calc'", Params: badMetricParams},
	})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}

	status, body := f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposal.ID+"/confirm", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("confirm: status %d, body %s", status, body)
	}
	var resp struct {
		Proposal struct {
			Status string `json:"status"`
			Steps  []struct {
				Status string `json:"status"`
				Result string `json:"result"`
			} `json:"steps"`
		} `json:"proposal"`
		Session struct {
			DraftRevisionID string `json:"draft_revision_id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("parse confirm response: %v", err)
	}
	if resp.Proposal.Status != "partial" {
		t.Fatalf("proposal.status = %q, want partial", resp.Proposal.Status)
	}
	if len(resp.Proposal.Steps) != 2 {
		t.Fatalf("expected 2 steps in the response, got %d", len(resp.Proposal.Steps))
	}
	if resp.Proposal.Steps[0].Status != "success" {
		t.Errorf("step 0 (create_dimension) status = %q, want success", resp.Proposal.Steps[0].Status)
	}
	if resp.Proposal.Steps[1].Status != "failed" || resp.Proposal.Steps[1].Result == "" {
		t.Errorf("step 1 (bad create_metric) status/result = %q/%q, want failed with a non-empty result", resp.Proposal.Steps[1].Status, resp.Proposal.Steps[1].Result)
	}
	draftRevID := resp.Session.DraftRevisionID
	if draftRevID == "" {
		t.Fatal("confirm did not create/report a draft revision")
	}
	if !dimensionExists(t, f.pool, f.modelID, draftRevID, "Region") {
		t.Error("the successful step's dimension is missing from the draft")
	}
	var badMetricCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid AND name='bogus_calc'`, f.modelID).Scan(&badMetricCount); err != nil {
		t.Fatalf("count bogus_calc: %v", err)
	}
	if badMetricCount != 0 {
		t.Error("the failed step's metric should not exist at all, not even half-created")
	}

	// The draft itself must not be poisoned: a second, all-valid proposal in
	// the same session confirms cleanly.
	dim2Params, _ := json.Marshal(map[string]any{"name": "Channel"})
	proposal2, err := pStore.CreateProposal(ctx, sess.ID, []aiassistant.ProposalStep{
		{Tool: "create_dimension", Description: "Create dimension 'Channel'", Params: dim2Params},
	})
	if err != nil {
		t.Fatalf("create proposal 2: %v", err)
	}
	status, body = f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposal2.ID+"/confirm", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("confirm 2 (after a partial failure): status %d, body %s — the draft must stay usable", status, body)
	}
	if !dimensionExists(t, f.pool, f.modelID, draftRevID, "Channel") {
		t.Error("second proposal's dimension is missing from the same draft")
	}
}

// TestConfirmProposal_AlreadyProcessedConflicts covers both routes that
// mutate proposal.status: confirming an already-confirmed proposal a
// second time, and rejecting a proposal that was already confirmed —
// both must 409, never silently re-execute or flip status backward.
func TestConfirmProposal_AlreadyProcessedConflicts(t *testing.T) {
	f := setupPromoteFixture(t)

	t.Run("confirming twice 409s on the second call", func(t *testing.T) {
		sessionID, proposalID := f.createSessionWithProposal(t, f.devID, "ConfirmTwice")
		if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil); status != http.StatusOK {
			t.Fatalf("confirm 1: status %d, body %s", status, body)
		}
		status, _ := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil)
		if status != http.StatusConflict {
			t.Errorf("confirm 2 (already executed): status %d, want 409", status)
		}
	})

	t.Run("rejecting an already-confirmed proposal 409s", func(t *testing.T) {
		sessionID, proposalID := f.createSessionWithProposal(t, f.devID, "ConfirmThenReject")
		if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil); status != http.StatusOK {
			t.Fatalf("confirm: status %d, body %s", status, body)
		}
		status, _ := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/reject", f.devSub, nil)
		if status != http.StatusConflict {
			t.Errorf("reject (already executed): status %d, want 409", status)
		}
	})

	t.Run("confirming an already-rejected proposal 409s", func(t *testing.T) {
		sessionID, proposalID := f.createSessionWithProposal(t, f.devID, "RejectThenConfirm")
		if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/reject", f.devSub, nil); status != http.StatusOK {
			t.Fatalf("reject: status %d, body %s", status, body)
		}
		status, _ := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil)
		if status != http.StatusConflict {
			t.Errorf("confirm (already rejected): status %d, want 409", status)
		}
	})
}
